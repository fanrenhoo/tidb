// Copyright 2021 PingCAP, Inc.
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

//go:build !codes
// +build !codes

package testkit

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/pingcap/errors"
	"github.com/pingcap/tidb/ddl"
	"github.com/pingcap/tidb/kv"
	"github.com/pingcap/tidb/parser/terror"
	"github.com/pingcap/tidb/session"
	"github.com/pingcap/tidb/sessionctx/variable"
	"github.com/pingcap/tidb/types"
	"github.com/pingcap/tidb/util/sqlexec"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/atomic"
)

var testKitIDGenerator atomic.Uint64

// TestKit is a utility to run sql test.
type TestKit struct {
	require *require.Assertions
	assert  *assert.Assertions
	t       testing.TB
	store   kv.Storage
	session session.Session
}

// NewTestKit returns a new *TestKit.
func NewTestKit(t testing.TB, store kv.Storage) *TestKit {
	return &TestKit{
		require: require.New(t),
		assert:  assert.New(t),
		t:       t,
		store:   store,
		session: newSession(t, store),
	}
}

// RefreshSession set a new session for the testkit
func (tk *TestKit) RefreshSession() {
	tk.session = newSession(tk.t, tk.store)
}

// SetSession set the session of testkit
func (tk *TestKit) SetSession(session session.Session) {
	tk.session = session
}

// Session return the session associated with the testkit
func (tk *TestKit) Session() session.Session {
	return tk.session
}

// MustExec executes a sql statement and asserts nil error.
func (tk *TestKit) MustExec(sql string, args ...interface{}) {
	res, err := tk.Exec(sql, args...)
	comment := fmt.Sprintf("sql:%s, %v, error stack %v", sql, args, errors.ErrorStack(err))
	tk.require.NoError(err, comment)

	if res != nil {
		tk.require.NoError(res.Close())
	}
}

// MustQuery query the statements and returns result rows.
// If expected result is set it asserts the query result equals expected result.
func (tk *TestKit) MustQuery(sql string, args ...interface{}) *Result {
	comment := fmt.Sprintf("sql:%s, args:%v", sql, args)
	rs, err := tk.Exec(sql, args...)
	tk.require.NoError(err, comment)
	tk.require.NotNil(rs, comment)
	return tk.ResultSetToResult(rs, comment)
}

// QueryToErr executes a sql statement and discard results.
func (tk *TestKit) QueryToErr(sql string, args ...interface{}) error {
	comment := fmt.Sprintf("sql:%s, args:%v", sql, args)
	res, err := tk.Exec(sql, args...)
	tk.require.NoError(err, comment)
	tk.require.NotNil(res, comment)
	_, resErr := session.GetRows4Test(context.Background(), tk.session, res)
	tk.require.NoError(res.Close())
	return resErr
}

// ResultSetToResult converts sqlexec.RecordSet to testkit.Result.
// It is used to check results of execute statement in binary mode.
func (tk *TestKit) ResultSetToResult(rs sqlexec.RecordSet, comment string) *Result {
	return tk.ResultSetToResultWithCtx(context.Background(), rs, comment)
}

// ResultSetToResultWithCtx converts sqlexec.RecordSet to testkit.Result.
func (tk *TestKit) ResultSetToResultWithCtx(ctx context.Context, rs sqlexec.RecordSet, comment string) *Result {
	rows, err := session.ResultSetToStringSlice(ctx, tk.session, rs)
	tk.require.NoError(err, comment)
	return &Result{rows: rows, comment: comment, assert: tk.assert, require: tk.require}
}

// HasPlan checks if the result execution plan contains specific plan.
func (tk *TestKit) HasPlan(sql string, plan string, args ...interface{}) bool {
	rs := tk.MustQuery("explain "+sql, args...)
	for i := range rs.rows {
		if strings.Contains(rs.rows[i][0], plan) {
			return true
		}
	}
	return false
}

// HasPlan4ExplainFor checks if the result execution plan contains specific plan.
func (tk *TestKit) HasPlan4ExplainFor(result *Result, plan string) bool {
	for i := range result.rows {
		if strings.Contains(result.rows[i][0], plan) {
			return true
		}
	}
	return false
}

// Exec executes a sql statement using the prepared stmt API
func (tk *TestKit) Exec(sql string, args ...interface{}) (sqlexec.RecordSet, error) {
	ctx := context.Background()
	if len(args) == 0 {
		sc := tk.session.GetSessionVars().StmtCtx
		prevWarns := sc.GetWarnings()
		stmts, err := tk.session.Parse(ctx, sql)
		if err != nil {
			return nil, errors.Trace(err)
		}
		warns := sc.GetWarnings()
		parserWarns := warns[len(prevWarns):]
		var rs0 sqlexec.RecordSet
		for i, stmt := range stmts {
			rs, err := tk.session.ExecuteStmt(ctx, stmt)
			if i == 0 {
				rs0 = rs
			}
			if err != nil {
				tk.session.GetSessionVars().StmtCtx.AppendError(err)
				return nil, errors.Trace(err)
			}
		}
		if len(parserWarns) > 0 {
			tk.session.GetSessionVars().StmtCtx.AppendWarnings(parserWarns)
		}
		return rs0, nil
	}

	stmtID, _, _, err := tk.session.PrepareStmt(sql)
	if err != nil {
		return nil, errors.Trace(err)
	}
	params := make([]types.Datum, len(args))
	for i := 0; i < len(params); i++ {
		params[i] = types.NewDatum(args[i])
	}
	rs, err := tk.session.ExecutePreparedStmt(ctx, stmtID, params)
	if err != nil {
		return nil, errors.Trace(err)
	}
	err = tk.session.DropPreparedStmt(stmtID)
	if err != nil {
		return nil, errors.Trace(err)
	}
	return rs, nil
}

// ExecToErr executes a sql statement and discard results.
func (tk *TestKit) ExecToErr(sql string, args ...interface{}) error {
	res, err := tk.Exec(sql, args...)
	if res != nil {
		tk.require.NoError(res.Close())
	}
	return err
}

func newSession(t testing.TB, store kv.Storage) session.Session {
	se, err := session.CreateSession4Test(store)
	require.NoError(t, err)
	se.SetConnectionID(testKitIDGenerator.Inc())
	return se
}

// RefreshConnectionID refresh the connection ID for session of the testkit
func (tk *TestKit) RefreshConnectionID() {
	if tk.session != nil {
		tk.session.SetConnectionID(testKitIDGenerator.Inc())
	}
}

// MustGetErrCode executes a sql statement and assert it's error code.
func (tk *TestKit) MustGetErrCode(sql string, errCode int) {
	_, err := tk.Exec(sql)
	tk.require.Error(err)
	originErr := errors.Cause(err)
	tErr, ok := originErr.(*terror.Error)
	tk.require.Truef(ok, "expect type 'terror.Error', but obtain '%T': %v", originErr, originErr)
	sqlErr := terror.ToSQLError(tErr)
	tk.require.Equalf(errCode, int(sqlErr.Code), "Assertion failed, origin err:\n  %v", sqlErr)
}

// MustGetErrMsg executes a sql statement and assert it's error message.
func (tk *TestKit) MustGetErrMsg(sql string, errStr string) {
	err := tk.ExecToErr(sql)
	tk.require.Error(err)
	tk.require.Equal(errStr, err.Error())
}

// MustUseIndex checks if the result execution plan contains specific index(es).
func (tk *TestKit) MustUseIndex(sql string, index string, args ...interface{}) bool {
	rs := tk.MustQuery("explain "+sql, args...)
	for i := range rs.rows {
		if strings.Contains(rs.rows[i][3], "index:"+index) {
			return true
		}
	}
	return false
}

// MustUseIndex4ExplainFor checks if the result execution plan contains specific index(es).
func (tk *TestKit) MustUseIndex4ExplainFor(result *Result, index string) bool {
	for i := range result.rows {
		// It depends on whether we enable to collect the execution info.
		if strings.Contains(result.rows[i][3], "index:"+index) {
			return true
		}
		if strings.Contains(result.rows[i][4], "index:"+index) {
			return true
		}
	}
	return false
}

// CheckExecResult checks the affected rows and the insert id after executing MustExec.
func (tk *TestKit) CheckExecResult(affectedRows, insertID int64) {
	tk.require.Equal(int64(tk.Session().AffectedRows()), affectedRows)
	tk.require.Equal(int64(tk.Session().LastInsertID()), insertID)
}

// WithPruneMode run test case under prune mode.
func WithPruneMode(tk *TestKit, mode variable.PartitionPruneMode, f func()) {
	tk.MustExec("set @@tidb_partition_prune_mode=`" + string(mode) + "`")
	tk.MustExec("set global tidb_partition_prune_mode=`" + string(mode) + "`")
	f()
}

// MockGC is used to make GC work in the test environment.
func MockGC(tk *TestKit) (string, string, string, func()) {
	originGC := ddl.IsEmulatorGCEnable()
	resetGC := func() {
		if originGC {
			ddl.EmulatorGCEnable()
		} else {
			ddl.EmulatorGCDisable()
		}
	}

	// disable emulator GC.
	// Otherwise emulator GC will delete table record as soon as possible after execute drop table ddl.
	ddl.EmulatorGCDisable()
	gcTimeFormat := "20060102-15:04:05 -0700 MST"
	timeBeforeDrop := time.Now().Add(0 - 48*60*60*time.Second).Format(gcTimeFormat)
	timeAfterDrop := time.Now().Add(48 * 60 * 60 * time.Second).Format(gcTimeFormat)
	safePointSQL := `INSERT HIGH_PRIORITY INTO mysql.tidb VALUES ('tikv_gc_safe_point', '%[1]s', '')
			       ON DUPLICATE KEY
			       UPDATE variable_value = '%[1]s'`
	// clear GC variables first.
	tk.MustExec("delete from mysql.tidb where variable_name in ( 'tikv_gc_safe_point','tikv_gc_enable' )")
	return timeBeforeDrop, timeAfterDrop, safePointSQL, resetGC
}

func containGlobal(rs *Result) bool {
	partitionNameCol := 2
	for i := range rs.rows {
		if strings.Contains(rs.rows[i][partitionNameCol], "global") {
			return true
		}
	}
	return false
}

// MustNoGlobalStats checks if there is no global stats.
func (tk *TestKit) MustNoGlobalStats(table string) bool {
	if containGlobal(tk.MustQuery("show stats_meta where table_name like '" + table + "'")) {
		return false
	}
	if containGlobal(tk.MustQuery("show stats_buckets where table_name like '" + table + "'")) {
		return false
	}
	if containGlobal(tk.MustQuery("show stats_histograms where table_name like '" + table + "'")) {
		return false
	}
	return true
}
