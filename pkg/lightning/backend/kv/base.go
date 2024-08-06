// Copyright 2023 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package kv

import (
	"context"
	"math/rand"

	"github.com/pingcap/errors"
	"github.com/pingcap/tidb/br/pkg/logutil"
	"github.com/pingcap/tidb/pkg/expression"
	"github.com/pingcap/tidb/pkg/kv"
	"github.com/pingcap/tidb/pkg/lightning/backend/encode"
	"github.com/pingcap/tidb/pkg/lightning/common"
	"github.com/pingcap/tidb/pkg/lightning/log"
	"github.com/pingcap/tidb/pkg/meta/autoid"
	"github.com/pingcap/tidb/pkg/parser/model"
	"github.com/pingcap/tidb/pkg/parser/mysql"
	"github.com/pingcap/tidb/pkg/sessionctx/variable"
	"github.com/pingcap/tidb/pkg/table"
	"github.com/pingcap/tidb/pkg/types"
	"github.com/pingcap/tidb/pkg/util/chunk"
	"github.com/pingcap/tidb/pkg/util/codec"
	"github.com/pingcap/tidb/pkg/util/redact"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

const (
	maxLogLength = 512 * 1024
)

// ExtraHandleColumnInfo is the column info of extra handle column.
var ExtraHandleColumnInfo = model.NewExtraHandleColInfo()

// GeneratedCol generated column info.
type GeneratedCol struct {
	// index of the column in the table
	Index int
	Expr  expression.Expression
}

// AutoIDConverterFn is a function to convert auto id.
type AutoIDConverterFn func(int64) int64

// RowArrayMarshaller wraps a slice of types.Datum for logging the content into zap.
type RowArrayMarshaller []types.Datum

var kindStr = [...]string{
	types.KindNull:          "null",
	types.KindInt64:         "int64",
	types.KindUint64:        "uint64",
	types.KindFloat32:       "float32",
	types.KindFloat64:       "float64",
	types.KindString:        "string",
	types.KindBytes:         "bytes",
	types.KindBinaryLiteral: "binary",
	types.KindMysqlDecimal:  "decimal",
	types.KindMysqlDuration: "duration",
	types.KindMysqlEnum:     "enum",
	types.KindMysqlBit:      "bit",
	types.KindMysqlSet:      "set",
	types.KindMysqlTime:     "time",
	types.KindInterface:     "interface",
	types.KindMinNotNull:    "min",
	types.KindMaxValue:      "max",
	types.KindRaw:           "raw",
	types.KindMysqlJSON:     "json",
}

// MarshalLogArray implements the zapcore.ArrayMarshaler interface
func (row RowArrayMarshaller) MarshalLogArray(encoder zapcore.ArrayEncoder) error {
	var totalLength = 0
	for _, datum := range row {
		kind := datum.Kind()
		var str string
		var err error
		switch kind {
		case types.KindNull:
			str = "NULL"
		case types.KindMinNotNull:
			str = "-inf"
		case types.KindMaxValue:
			str = "+inf"
		default:
			str, err = datum.ToString()
			if err != nil {
				return err
			}
		}
		if len(str) > maxLogLength {
			str = str[0:1024] + " (truncated)"
		}
		totalLength += len(str)
		if totalLength >= maxLogLength {
			encoder.AppendString("The row has been truncated, and the log has exited early.")
			return nil
		}
		if err := encoder.AppendObject(zapcore.ObjectMarshalerFunc(func(enc zapcore.ObjectEncoder) error {
			enc.AddString("kind", kindStr[kind])
			enc.AddString("val", redact.Value(str))
			return nil
		})); err != nil {
			return err
		}
	}
	return nil
}

// BaseKVEncoder encodes a row into a KV pair.
type BaseKVEncoder struct {
	GenCols         []GeneratedCol
	SessionCtx      *Session
	table           table.Table
	Columns         []*table.Column
	AutoRandomColID int64
	// convert auto id for shard rowid or auto random id base on row id generated by lightning
	AutoIDFn AutoIDConverterFn

	logger      *zap.Logger
	recordCache []types.Datum
}

// NewBaseKVEncoder creates a new BaseKVEncoder.
func NewBaseKVEncoder(config *encode.EncodingConfig) (*BaseKVEncoder, error) {
	meta := config.Table.Meta()
	cols := config.Table.Cols()
	se := NewSession(&config.SessionOptions, config.Logger)
	// Set CommonAddRecordCtx to session to reuse the slices and BufStore in AddRecord

	var autoRandomColID int64
	autoIDFn := func(id int64) int64 { return id }
	if meta.ContainsAutoRandomBits() {
		col := common.GetAutoRandomColumn(meta)
		autoRandomColID = col.ID

		shardFmt := autoid.NewShardIDFormat(&col.FieldType, meta.AutoRandomBits, meta.AutoRandomRangeBits)
		shard := rand.New(rand.NewSource(config.AutoRandomSeed)).Int63()
		autoIDFn = func(id int64) int64 {
			return shardFmt.Compose(shard, id)
		}
	} else if meta.ShardRowIDBits > 0 {
		rd := rand.New(rand.NewSource(config.AutoRandomSeed)) // nolint:gosec
		mask := int64(1)<<meta.ShardRowIDBits - 1
		shift := autoid.RowIDBitLength - meta.ShardRowIDBits - 1
		autoIDFn = func(id int64) int64 {
			rd.Seed(id)
			shardBits := (int64(rd.Uint32()) & mask) << shift
			return shardBits | id
		}
	}

	// collect expressions for evaluating stored generated columns
	genCols, err := CollectGeneratedColumns(se, meta, cols)
	if err != nil {
		return nil, errors.Annotate(err, "failed to parse generated column expressions")
	}
	return &BaseKVEncoder{
		GenCols:         genCols,
		SessionCtx:      se,
		table:           config.Table,
		Columns:         cols,
		AutoRandomColID: autoRandomColID,
		AutoIDFn:        autoIDFn,
		logger:          config.Logger.Logger,
	}, nil
}

// GetOrCreateRecord returns a record slice from the cache if possible, otherwise creates a new one.
func (e *BaseKVEncoder) GetOrCreateRecord() []types.Datum {
	if e.recordCache != nil {
		return e.recordCache
	}
	return make([]types.Datum, 0, len(e.Columns)+1)
}

// Record2KV converts a row into a KV pair.
func (e *BaseKVEncoder) Record2KV(record, originalRow []types.Datum, rowID int64) (*Pairs, error) {
	_, err := e.AddRecord(record)
	if err != nil {
		e.logger.Error("kv encode failed",
			zap.Array("originalRow", RowArrayMarshaller(originalRow)),
			zap.Array("convertedRow", RowArrayMarshaller(record)),
			log.ShortError(err),
		)
		return nil, errors.Trace(err)
	}
	kvPairs := e.SessionCtx.TakeKvPairs()
	for i := 0; i < len(kvPairs.Pairs); i++ {
		var encoded [9]byte // The max length of encoded int64 is 9.
		kvPairs.Pairs[i].RowID = codec.EncodeComparableVarint(encoded[:0], rowID)
	}
	e.recordCache = record[:0]
	return kvPairs, nil
}

// AddRecord adds a record into encoder
func (e *BaseKVEncoder) AddRecord(record []types.Datum) (kv.Handle, error) {
	return e.table.AddRecord(e.SessionCtx.GetTableCtx(), record, table.DupKeyCheckSkip)
}

// TableAllocators returns the allocators of the table
func (e *BaseKVEncoder) TableAllocators() autoid.Allocators {
	return e.table.Allocators(e.SessionCtx.GetTableCtx())
}

// TableMeta returns the meta of the table
func (e *BaseKVEncoder) TableMeta() *model.TableInfo {
	return e.table.Meta()
}

// ProcessColDatum processes the datum of a column.
func (e *BaseKVEncoder) ProcessColDatum(col *table.Column, rowID int64, inputDatum *types.Datum) (types.Datum, error) {
	value, err := e.getActualDatum(col, rowID, inputDatum)
	if err != nil {
		return value, err
	}

	if e.IsAutoRandomCol(col.ToInfo()) {
		meta := e.table.Meta()
		shardFmt := autoid.NewShardIDFormat(&col.FieldType, meta.AutoRandomBits, meta.AutoRandomRangeBits)
		// this allocator is the same as the allocator in table importer, i.e. PanickingAllocators. below too.
		alloc := e.TableAllocators().Get(autoid.AutoRandomType)
		if err := alloc.Rebase(context.Background(), value.GetInt64()&shardFmt.IncrementalMask(), false); err != nil {
			return value, errors.Trace(err)
		}
	}
	if IsAutoIncCol(col.ToInfo()) {
		// same as RowIDAllocType, since SepAutoInc is always false when initializing allocators of Table.
		alloc := e.TableAllocators().Get(autoid.AutoIncrementType)
		if err := alloc.Rebase(context.Background(), GetAutoRecordID(value, &col.FieldType), false); err != nil {
			return value, errors.Trace(err)
		}
	}
	return value, nil
}

func (e *BaseKVEncoder) getActualDatum(col *table.Column, rowID int64, inputDatum *types.Datum) (types.Datum, error) {
	var (
		value types.Datum
		err   error
	)

	isBadNullValue := false
	if inputDatum != nil {
		value, err = table.CastValue(e.SessionCtx, *inputDatum, col.ToInfo(), false, false)
		if err != nil {
			return value, err
		}
		if err := col.CheckNotNull(&value, 0); err == nil {
			return value, nil // the most normal case
		}
		isBadNullValue = true
	}
	// handle special values
	switch {
	case IsAutoIncCol(col.ToInfo()):
		// we still need a conversion, e.g. to catch overflow with a TINYINT column.
		value, err = table.CastValue(e.SessionCtx,
			types.NewIntDatum(rowID), col.ToInfo(), false, false)
	case e.IsAutoRandomCol(col.ToInfo()):
		var val types.Datum
		realRowID := e.AutoIDFn(rowID)
		if mysql.HasUnsignedFlag(col.GetFlag()) {
			val = types.NewUintDatum(uint64(realRowID))
		} else {
			val = types.NewIntDatum(realRowID)
		}
		value, err = table.CastValue(e.SessionCtx, val, col.ToInfo(), false, false)
	case col.IsGenerated():
		// inject some dummy value for gen col so that MutRowFromDatums below sees a real value instead of nil.
		// if MutRowFromDatums sees a nil it won't initialize the underlying storage and cause SetDatum to panic.
		value = types.GetMinValue(&col.FieldType)
	case isBadNullValue:
		err = col.HandleBadNull(e.SessionCtx.Vars.StmtCtx.ErrCtx(), &value, 0)
	default:
		// copy from the following GetColDefaultValue function, when this is true it will use getColDefaultExprValue
		if col.DefaultIsExpr {
			// the expression rewriter requires a non-nil TxnCtx.
			e.SessionCtx.Vars.TxnCtx = new(variable.TransactionContext)
			defer func() {
				e.SessionCtx.Vars.TxnCtx = nil
			}()
		}
		value, err = table.GetColDefaultValue(e.SessionCtx.GetExprCtx(), col.ToInfo())
	}
	return value, err
}

// IsAutoRandomCol checks if the column is auto random column.
func (e *BaseKVEncoder) IsAutoRandomCol(col *model.ColumnInfo) bool {
	return e.table.Meta().ContainsAutoRandomBits() && col.ID == e.AutoRandomColID
}

// EvalGeneratedColumns evaluates the generated columns.
func (e *BaseKVEncoder) EvalGeneratedColumns(record []types.Datum,
	cols []*table.Column) (errCol *model.ColumnInfo, err error) {
	return evalGeneratedColumns(e.SessionCtx, record, cols, e.GenCols)
}

// LogKVConvertFailed logs the error when converting a row to KV pair failed.
func (e *BaseKVEncoder) LogKVConvertFailed(row []types.Datum, j int, colInfo *model.ColumnInfo, err error) error {
	var original types.Datum
	if 0 <= j && j < len(row) {
		original = row[j]
		row = row[j : j+1]
	}

	e.logger.Error("kv convert failed",
		zap.Array("original", RowArrayMarshaller(row)),
		zap.Int("originalCol", j),
		zap.String("colName", colInfo.Name.O),
		zap.Stringer("colType", &colInfo.FieldType),
		log.ShortError(err),
	)

	if len(original.GetString()) >= maxLogLength {
		originalPrefix := original.GetString()[0:1024] + " (truncated)"
		e.logger.Error("failed to convert kv value", logutil.RedactAny("origVal", originalPrefix),
			zap.Stringer("fieldType", &colInfo.FieldType), zap.String("column", colInfo.Name.O),
			zap.Int("columnID", j+1))
	} else {
		e.logger.Error("failed to convert kv value", logutil.RedactAny("origVal", original.GetValue()),
			zap.Stringer("fieldType", &colInfo.FieldType), zap.String("column", colInfo.Name.O),
			zap.Int("columnID", j+1))
	}
	return errors.Annotatef(
		err,
		"failed to cast value as %s for column `%s` (#%d)", &colInfo.FieldType, colInfo.Name.O, j+1,
	)
}

// LogEvalGenExprFailed logs the error when evaluating the generated column expression failed.
func (e *BaseKVEncoder) LogEvalGenExprFailed(row []types.Datum, colInfo *model.ColumnInfo, err error) error {
	e.logger.Error("kv convert failed: cannot evaluate generated column expression",
		zap.Array("original", RowArrayMarshaller(row)),
		zap.String("colName", colInfo.Name.O),
		log.ShortError(err),
	)

	return errors.Annotatef(
		err,
		"failed to evaluate generated column expression for column `%s`",
		colInfo.Name.O,
	)
}

// TruncateWarns resets the warnings in session context.
func (e *BaseKVEncoder) TruncateWarns() {
	e.SessionCtx.Vars.StmtCtx.TruncateWarnings(0)
}

func evalGeneratedColumns(se *Session, record []types.Datum, cols []*table.Column,
	genCols []GeneratedCol) (errCol *model.ColumnInfo, err error) {
	mutRow := chunk.MutRowFromDatums(record)
	for _, gc := range genCols {
		col := cols[gc.Index].ToInfo()
		evaluated, err := gc.Expr.Eval(se.GetExprCtx().GetEvalCtx(), mutRow.ToRow())
		if err != nil {
			return col, err
		}
		value, err := table.CastValue(se, evaluated, col, false, false)
		if err != nil {
			return col, err
		}
		mutRow.SetDatum(gc.Index, value)
		record[gc.Index] = value
	}
	return nil, nil
}
