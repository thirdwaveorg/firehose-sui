package codec

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	pbsui "github.com/apocentre/firehose-sui/pb/sf/sui/type/v1"
	pbbstream "github.com/streamingfast/bstream/pb/sf/bstream/v1"
	firecore "github.com/streamingfast/firehose-core"
	"github.com/streamingfast/firehose-core/node-manager/mindreader"
	"github.com/streamingfast/logging"
	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"
)

// ConsoleReader is what reads the `geth` output directly. It builds
// up some LogEntry objects. See `LogReader to read those entries .
type ConsoleReader struct {
	lines  chan string
	close  func()
	done   chan interface{}
	logger *zap.Logger
	encoder  firecore.BlockEncoder

	activeBlockStartTime time.Time
	activeBlock          *pbsui.CheckpointData
	// Chain Identifier is the digest of the genesis checkpoint
	chainID              string
	initRead             bool
	stats                *consoleReaderStats
}

func NewConsoleReader(
	lines chan string,
	blockEncoder firecore.BlockEncoder,
	logger *zap.Logger,
	tracer logging.Tracer,
) (mindreader.ConsolerReader, error) {
	l := &ConsoleReader{
		lines:  lines,
		close:  func() {},
		encoder: blockEncoder,
		done:   make(chan interface{}),
		logger: logger,
		stats: newConsoleReaderStats(),
	}

	l.stats.StartPeriodicLogToZap(context.Background(), logger, 30*time.Second)

	return l, nil
}

func (r *ConsoleReader) Done() <-chan interface{} {
	return r.done
}

func (r *ConsoleReader) Close() {
	r.stats.StopPeriodicLogToZap()

	r.close()
}

func (r *ConsoleReader) ReadBlock() (out *pbbstream.Block, err error) {
	block, err := r.next()
	if err != nil {
		return nil, err
	}

	if block == nil {
		return nil, fmt.Errorf("console reader read a nil *pbbstream.Block, this is invalid")
	}

	return block.(*pbbstream.Block), nil
}

const (
	LogPrefix         = "FIRE"
	LogInit           = "INIT"
	LogCheckpoint     = "CHECKPOINT"
	LogBlockStart     = "BLOCK_START"
	LogTrx            = "TRX"
	LogObjChange      = "OBJ_CHANGE"
	LogEvent          = "EVT"
	LogDisplayUpdate  = "DSP_UPDATE"
	LogBlockEnd       = "BLOCK_END"
)

func (r *ConsoleReader) next() (out interface{}, err error) {
	for line := range r.lines {
		if !strings.HasPrefix(line, LogPrefix) {
			continue
		}

		// This code assumes that distinct element do not contains space. This can happen
		// for example when exchanging JSON object (although we strongly discourage usage of
		// JSON, use serialized Protobuf object). If you happen to have spaces in the last element,
		// refactor the code here to avoid the split and perform the split in the line handler directly
		// instead.
		tokens := strings.Split(line[len(LogPrefix)+1:], " ")
		if len(tokens) < 2 {
			return nil, fmt.Errorf("invalid log line %q, expecting at least two tokens", line)
		}

		if !r.initRead {
			if tokens[0] == LogInit {
				if err := r.readInit(tokens[1:]); err != nil {
					return nil, lineError(line, err)
				}
			} else {
				r.logger.Warn("received Firehose log line but we did not see 'FIRE INIT' yet, skipping", zap.String("prefix", tokens[0]))
			}

			continue
		}

		// Order the case from most occurring line prefix to least occurring
		switch tokens[0] {
		case LogCheckpoint:
			err = r.readCheckpointOverview(tokens[1:])
		case LogTrx:
			err = r.readTransactionBlock(tokens[1:])
		case LogObjChange:
			err = r.readTransactionObjectChange(tokens[1:])
		case LogEvent:
			err = r.readEvent(tokens[1:])
		case LogDisplayUpdate:
			err = r.readDisplayUpdate(tokens[1:])
		case LogBlockStart:
			err = r.readBlockStart(tokens[1:])
		case LogBlockEnd:
			// This end the execution of the reading loop as we have a full block here
			block, err := r.readBlockEnd(tokens[1:])
			if err != nil {
				return nil, lineError(line, err)
			}

			return block, nil
		case LogInit:
			err = fmt.Errorf("received INIT line while one has already been read")
		default:
			if r.logger.Core().Enabled(zap.DebugLevel) {
				r.logger.Debug("skipping unknown firehose log line", zap.String("line", line))
			}

			continue
		}

		if err != nil {
			return nil, lineError(line, err)
		}
	}

	r.logger.Info("lines channel has been closed")
	return nil, io.EOF
}

// Format:
// FIRE INIT <client_name> <client_version> <fork> <firehose_major> <firehose_minor> <chain_id>
func (r *ConsoleReader) readInit(params []string) error {
	if err := validateVariableChunk(params, 6, 7); err != nil {
		return fmt.Errorf("invalid log line length: %w", err)
	}

	clientName := params[0]
	clientVersion := params[1]
	fork := params[2]

	firehoseMajor, err := strconv.ParseUint(params[3], 10, 64)
	if err != nil {
		return fmt.Errorf("invalid firehose major version %q: %w", params[3], err)
	}

	firehoseMinor, err := strconv.ParseUint(params[3], 10, 64)
	if err != nil {
		return fmt.Errorf("invalid firehose minor version %q: %w", params[4], err)
	}

	if firehoseMajor != 0 {
		return fmt.Errorf("only able to consume firehose format with major version 0, got %d", firehoseMajor)
	}

	chainIDString := ""
	if len(params) == 6 {
		chainIDString = params[5]
	} else {
		chainIDString = params[6]
	}

	r.logger.Info("initialized console reader correclty",
		zap.String("client_name", clientName),
		zap.String("client_version", clientVersion),
		zap.String("fork", fork),
		zap.Uint64("firehose_major", firehoseMajor),
		zap.Uint64("firehose_minor", firehoseMinor),
		zap.String("chain_id", chainIDString),
	)

	r.chainID = chainIDString
	r.initRead = true

	return nil
}

// Format:
// FIRE BLOCK_START <height>
func (r *ConsoleReader) readBlockStart(params []string) error {
	if err := validateChunk(params, 1); err != nil {
		return fmt.Errorf("invalid BLOCK_START line: %w", err)
	}

	height, err := strconv.ParseUint(params[0], 10, 64)
	if err != nil {
		return fmt.Errorf(`invalid BLOCK_START "height" param: %w`, err)
	}

	if r.activeBlock != nil {
		r.logger.Info("received BLOCK_START while one is already active, resetting active block and starting over",
			zap.Uint64("previous_active_block_height", r.activeBlock.Checkpoint.SequenceNumber),
			zap.Uint64("new_active_block_height", height),
		)
	}

	r.activeBlockStartTime = time.Now()
	r.activeBlock = &pbsui.CheckpointData {}

	return nil
}

func(r *ConsoleReader) validate(params []string, valueType string) ([]byte, error) {
	if err := validateChunk(params, 1); err != nil {
		return nil, fmt.Errorf("invalid log line length: %w", err)
	}

	if r.activeBlock == nil {
		return nil, fmt.Errorf("no active block in progress when reading %s", valueType)
	}

	out, err := base64.StdEncoding.DecodeString(params[0])
	if err != nil {
		return nil, fmt.Errorf("read %s in block %d: invalid base64 value: %w", valueType, r.activeBlock.GetFirehoseBlockNumber(), err)
	}

	return out, nil
}

// Format:
// FIRE CHECKPOINT <pbsui.Checkpoint>
func(r * ConsoleReader) readCheckpointOverview(params []string) error {
	out, err := r.validate(params, "CHECKPOINT")
	if err != nil {
		return err
	}

	checkpoint := &pbsui.Checkpoint{}
	if err := proto.Unmarshal(out, checkpoint); err != nil {
		return fmt.Errorf("read CHECKPOINT in block %d: invalid proto: %w", r.activeBlock.GetFirehoseBlockNumber(), err)
	}

	r.activeBlock.Checkpoint = checkpoint

	return nil
}

// Format:
// FIRE TRX <pbsui.Transaction>
func (r *ConsoleReader) readTransactionBlock(params []string) error {
	out, err := r.validate(params, "TRX")
	if err != nil {
		return err
	}

	transaction := &pbsui.Transaction{}
	if err := proto.Unmarshal(out, transaction); err != nil {
		return fmt.Errorf("read TRX in block %d: invalid proto: %w", r.activeBlock.GetFirehoseBlockNumber(), err)
	}

	r.activeBlock.Transactions = append(r.activeBlock.Transactions, transaction)

	return nil
}

// Format:
// FIRE OBJ_CHANGE <pbsui.TransactionObjectChange>
func (r *ConsoleReader) readTransactionObjectChange(params []string) error {
	out, err := r.validate(params, "OBJ_CHANGE")
	if err != nil {
		return err
	}

	tx_object_change := &pbsui.TransactionObjectChange{}
	if err := proto.Unmarshal(out, tx_object_change); err != nil {
		return fmt.Errorf("read OBJ_CHANGE in block %d: invalid proto: %w", r.activeBlock.GetFirehoseBlockNumber(), err)
	}

	r.activeBlock.ObjectChange = tx_object_change

	return nil
}

// Format:
// FIRE EVT <pbsui.IndexedEvent>
func (r *ConsoleReader) readEvent(params []string) error {
	out, err := r.validate(params, "EVT")
	if err != nil {
		return err
	}

	event := &pbsui.IndexedEvent{}
	if err := proto.Unmarshal(out, event); err != nil {
		return fmt.Errorf("read EVT in block %d: invalid proto: %w", r.activeBlock.GetFirehoseBlockNumber(), err)
	}

	r.activeBlock.Events = append(r.activeBlock.Events, event)

	return nil
}

// Format:
// FIRE EVT <pbsui.StoredDisplay>
func (r *ConsoleReader) readDisplayUpdate(params []string) error {
	out, err := r.validate(params, "DSP_UPDATE")
	if err != nil {
		return err
	}

	display_update := &pbsui.StoredDisplay{}
	if err := proto.Unmarshal(out, display_update); err != nil {
		return fmt.Errorf("read DSP_UPDATE in block %d: invalid proto: %w", r.activeBlock.GetFirehoseBlockNumber(), err)
	}

	r.activeBlock.DisplayUpdates = append(r.activeBlock.DisplayUpdates, display_update)

	return nil
}

// Format:
// FIRE BLOCK_END <height>
func (r *ConsoleReader) readBlockEnd(params []string) (*pbbstream.Block, error) {
	if err := validateChunk(params, 1); err != nil {
		return nil, fmt.Errorf("invalid BLOCK_END line: %w", err)
	}

	height, err := strconv.ParseUint(params[0], 10, 64)
	if err != nil {
		return nil, fmt.Errorf(`invalid BLOCK_END "height" param: %w`, err)
	}

	if r.activeBlock == nil {
		return nil, fmt.Errorf("no active block in progress when reading BLOCK_END")
	}

	if r.activeBlock.GetFirehoseBlockNumber() != height {
		return nil, fmt.Errorf("active block's height %d does not match BLOCK_END received height %d", r.activeBlock.GetFirehoseBlockNumber(), height)
	}

	if len(r.activeBlock.Transactions) == 0 {
		return nil, fmt.Errorf("active block height %d does not contain any transaction", r.activeBlock.GetFirehoseBlockNumber())
	}

	r.stats.blockRate.Inc()
	r.stats.transactionRate.IncBy(int64(len(r.activeBlock.Transactions)))
	r.stats.blockAverageParseTime.AddElapsedTime(r.activeBlockStartTime)
	r.stats.lastBlock = r.activeBlock.AsRef()

	r.logger.Debug("console reader node block",
		zap.String("id", r.activeBlock.GetFirehoseBlockID()),
		zap.Uint64("height", r.activeBlock.GetFirehoseBlockNumber()),
		zap.Time("timestamp", r.activeBlock.GetFirehoseBlockTime()),
	)

	block := r.activeBlock
	r.resetActiveBlock()

	bstreamBlock, err := r.encoder.Encode(firecore.BlockEnveloppe{Block: block, LIBNum: block.GetFirehoseBlockLIBNum()})
	if err != nil {
		return nil, err
	}

	return bstreamBlock, nil
}

func (r *ConsoleReader) resetActiveBlock() {
	r.activeBlock = nil
	r.activeBlockStartTime = time.Time{}
}

func validateChunk(params []string, count int) error {
	if len(params) != count {
		return fmt.Errorf("%d fields required but found %d", count, len(params))
	}
	return nil
}

func validateVariableChunk(params []string, counts ...int) error {
	for _, validCount := range counts {
		if len(params) == validCount {
			return nil
		}
	}

	countStrings := make([]string, len(counts))
	for i, validCount := range counts {
		countStrings[i] = strconv.FormatUint(uint64(validCount), 10)
	}

	return fmt.Errorf("%s fields required but found %d", strings.Join(countStrings, " or "), len(params))
}

func lineError(line string, source error) error {
	return fmt.Errorf("%w (on line %q)", source, line)
}
