package taosRestful

import (
	"compress/gzip"
	"context"
	"database/sql/driver"
	"encoding/base64"
	"errors"
	"fmt"
	jsonitor "github.com/json-iterator/go"
	"github.com/taosdata/driver-go/v3/common"
	taosErrors "github.com/taosdata/driver-go/v3/errors"
	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/syncx"
	"github.com/zeromicro/go-zero/core/timex"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

var jsonI = jsonitor.ConfigCompatibleWithStandardLibrary

const defaultSlowThreshold = time.Millisecond * 500

var slowThreshold = syncx.ForAtomicDuration(defaultSlowThreshold)

// SetSlowThreshold sets the slow threshold.
func SetSlowThreshold(threshold time.Duration) {
	slowThreshold.Set(threshold)
}

type taosConn struct {
	cfg            *config
	client         *http.Client
	url            *url.URL
	header         map[string][]string
	readBufferSize int
}

func newTaosConn(cfg *config) (*taosConn, error) {
	readBufferSize := cfg.readBufferSize
	if readBufferSize <= 0 {
		readBufferSize = 4 << 10
	}
	tc := &taosConn{cfg: cfg, readBufferSize: readBufferSize}
	tc.client = &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   30 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
			DisableCompression:    cfg.disableCompression,
		},
	}
	path := "/rest/sql"
	if len(cfg.dbName) != 0 {
		path = fmt.Sprintf("%s/%s", path, cfg.dbName)
	}
	tc.url = &url.URL{
		Scheme: cfg.net,
		Host:   fmt.Sprintf("%s:%d", cfg.addr, cfg.port),
		Path:   path,
	}
	tc.header = map[string][]string{
		"Connection": {"keep-alive"},
	}
	if cfg.token != "" {
		tc.url.RawQuery = fmt.Sprintf("token=%s", cfg.token)
	} else {
		basic := base64.StdEncoding.EncodeToString([]byte(cfg.user + ":" + cfg.passwd))
		tc.header["Authorization"] = []string{fmt.Sprintf("Basic %s", basic)}
	}
	if !cfg.disableCompression {
		tc.header["Accept-Encoding"] = []string{"gzip"}
	}
	return tc, nil
}

func (tc *taosConn) Begin() (driver.Tx, error) {
	return nil, &taosErrors.TaosError{Code: 0xffff, ErrStr: "restful does not support transaction"}
}

func (tc *taosConn) Close() (err error) {
	tc.client = nil
	tc.url = nil
	tc.cfg = nil
	tc.header = nil
	return nil
}

func (tc *taosConn) Prepare(query string) (driver.Stmt, error) {
	return nil, &taosErrors.TaosError{Code: 0xffff, ErrStr: "restful does not support stmt"}
}

func (tc *taosConn) Exec(query string, args []driver.Value) (driver.Result, error) {
	if len(args) != 0 {
		if !tc.cfg.interpolateParams {
			return nil, driver.ErrSkip
		}
		// try to interpolate the parameters to save extra round trips for preparing and closing a statement
		prepared, err := common.InterpolateParams(query, args)
		if err != nil {
			return nil, err
		}
		query = prepared
	}
	result, err := tc.taosQuery(context.TODO(), query, 512)
	if err != nil {
		return nil, err
	}
	if len(result.Data) != 1 || len(result.Data[0]) != 1 {
		return nil, errors.New("wrong result")
	}
	return driver.RowsAffected(result.Data[0][0].(int32)), nil
}

func (tc *taosConn) Query(query string, args []driver.Value) (driver.Rows, error) {
	if len(args) != 0 {
		if !tc.cfg.interpolateParams {
			return nil, driver.ErrSkip
		}
		// try client-side prepare to reduce round trip
		prepared, err := common.InterpolateParams(query, args)
		if err != nil {
			return nil, err
		}
		query = prepared
	}
	result, err := tc.taosQuery(context.TODO(), query, tc.readBufferSize)
	if err != nil {
		return nil, err
	}
	if result == nil {
		return nil, errors.New("wrong result")
	}
	// Read Result
	rs := &rows{
		result: result,
	}
	return rs, err
}

func (tc *taosConn) Ping(ctx context.Context) (err error) {
	return nil
}

func (tc *taosConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	return nil, &taosErrors.TaosError{Code: 0xffff, ErrStr: "restful does not support transaction"}
}

func (tc *taosConn) taosQuery(ctx context.Context, sql string, bufferSize int) (*common.TDEngineRestfulResp, error) {
	body := ioutil.NopCloser(strings.NewReader(sql))
	req := &http.Request{
		Method:     http.MethodPost,
		URL:        tc.url,
		Proto:      "HTTP/1.1",
		ProtoMajor: 1,
		ProtoMinor: 1,
		Header:     tc.header,
		Body:       body,
		Host:       tc.url.Host,
	}
	if ctx != nil {
		req = req.WithContext(ctx)
	}
	startTime := timex.Now()
	duration := timex.Since(startTime)
	resp, err := tc.client.Do(req)
	if duration > slowThreshold.Load() {
		logx.WithContext(ctx).WithDuration(duration).Slowf("[SQL] taosQuery: slowcall - %s", sql)
	} else {
		logx.WithContext(ctx).WithDuration(duration).Infof("sql taosQuery: %s", sql)
	}
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("server response: %s - %s", resp.Status, string(body))
	}
	respBody := resp.Body
	if !tc.cfg.disableCompression && EqualFold(resp.Header.Get("Content-Encoding"), "gzip") {
		respBody, err = gzip.NewReader(resp.Body)
		if err != nil {
			return nil, err
		}
	}
	data, err := marshalBody(respBody, bufferSize)
	if err != nil {
		return nil, err
	}
	if data.Code != 0 {
		return nil, taosErrors.NewError(data.Code, data.Desc)
	}
	return data, nil
}

const HTTPDTimeFormat = "2006-01-02T15:04:05.999999999-0700"

func marshalBody(body io.Reader, bufferSize int) (*common.TDEngineRestfulResp, error) {
	var result common.TDEngineRestfulResp
	iter := jsonI.BorrowIterator(make([]byte, bufferSize))
	defer jsonI.ReturnIterator(iter)
	iter.Reset(body)
	timeFormat := time.RFC3339Nano
	iter.ReadObjectCB(func(iter *jsonitor.Iterator, s string) bool {
		switch s {
		case "code":
			result.Code = iter.ReadInt()
		case "desc":
			result.Desc = iter.ReadString()
		case "column_meta":
			iter.ReadArrayCB(func(iter *jsonitor.Iterator) bool {
				index := 0
				iter.ReadArrayCB(func(iter *jsonitor.Iterator) bool {
					switch index {
					case 0:
						result.ColNames = append(result.ColNames, iter.ReadString())
						index = 1
					case 1:
						typeStr := iter.ReadString()
						t, exist := common.NameTypeMap[typeStr]
						if exist {
							result.ColTypes = append(result.ColTypes, t)
						} else {
							iter.ReportError("unsupported type in column_meta", typeStr)
						}
						index = 2
					case 2:
						result.ColLength = append(result.ColLength, iter.ReadInt64())
						index = 0
					}
					return true
				})
				return true
			})
		case "data":
			columnCount := len(result.ColTypes)
			column := 0
			iter.ReadArrayCB(func(iter *jsonitor.Iterator) bool {
				column = 0
				var row = make([]driver.Value, columnCount)
				iter.ReadArrayCB(func(iter *jsonitor.Iterator) bool {
					defer func() {
						column += 1
					}()
					columnType := result.ColTypes[column]
					if columnType == common.TSDB_DATA_TYPE_JSON {
						row[column] = iter.SkipAndReturnBytes()
						return true
					}
					if iter.ReadNil() {
						row[column] = nil
						return true
					}
					var err error
					switch columnType {
					case common.TSDB_DATA_TYPE_NULL:
						iter.Skip()
						row[column] = nil
					case common.TSDB_DATA_TYPE_BOOL:
						row[column] = iter.ReadAny().ToBool()
					case common.TSDB_DATA_TYPE_TINYINT:
						row[column] = iter.ReadInt8()
					case common.TSDB_DATA_TYPE_SMALLINT:
						row[column] = iter.ReadInt16()
					case common.TSDB_DATA_TYPE_INT:
						row[column] = iter.ReadInt32()
					case common.TSDB_DATA_TYPE_BIGINT:
						row[column] = iter.ReadInt64()
					case common.TSDB_DATA_TYPE_FLOAT:
						row[column] = iter.ReadFloat32()
					case common.TSDB_DATA_TYPE_DOUBLE:
						row[column] = iter.ReadFloat64()
					case common.TSDB_DATA_TYPE_BINARY:
						row[column] = iter.ReadString()
					case common.TSDB_DATA_TYPE_TIMESTAMP:
						b := iter.ReadString()
						row[column], err = time.Parse(timeFormat, b)
						if err != nil {
							iter.ReportError("parse time", err.Error())
						}
					case common.TSDB_DATA_TYPE_NCHAR:
						row[column] = iter.ReadString()
					case common.TSDB_DATA_TYPE_UTINYINT:
						row[column] = iter.ReadUint8()
					case common.TSDB_DATA_TYPE_USMALLINT:
						row[column] = iter.ReadUint16()
					case common.TSDB_DATA_TYPE_UINT:
						row[column] = iter.ReadUint32()
					case common.TSDB_DATA_TYPE_UBIGINT:
						row[column] = iter.ReadUint64()
					default:
						row[column] = nil
						iter.Skip()
					}
					return iter.Error == nil
				})
				if iter.Error != nil {
					return false
				}
				result.Data = append(result.Data, row)
				return true
			})
		case "rows":
			result.Rows = iter.ReadInt()
		default:
			iter.Skip()
		}
		return iter.Error == nil
	})
	if iter.Error != nil && iter.Error != io.EOF {
		return nil, iter.Error
	}
	return &result, nil
}

// EqualFold is strings.EqualFold, ASCII only. It reports whether s and t
// are equal, ASCII-case-insensitively.
func EqualFold(s, t string) bool {
	if len(s) != len(t) {
		return false
	}
	for i := 0; i < len(s); i++ {
		if lower(s[i]) != lower(t[i]) {
			return false
		}
	}
	return true
}

// lower returns the ASCII lowercase version of b.
func lower(b byte) byte {
	if 'A' <= b && b <= 'Z' {
		return b + ('a' - 'A')
	}
	return b
}
