// Copyright 2019 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package jobs

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/cockroachdb/cockroach/pkg/jobs/jobspb"
	"github.com/cockroachdb/cockroach/pkg/sql/isql"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/sessiondata"
	"github.com/cockroachdb/cockroach/pkg/sql/sqlliveness"
	"github.com/cockroachdb/cockroach/pkg/util/hlc"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/protoutil"
	"github.com/cockroachdb/cockroach/pkg/util/timeutil"
	"github.com/cockroachdb/cockroach/pkg/util/tracing"
	"github.com/cockroachdb/errors"
)

// UpdateFn is the callback passed to Job.Update. It is called from the context
// of a transaction and is passed the current metadata for the job. The callback
// can modify metadata using the JobUpdater and the changes will be persisted
// within the same transaction.
//
// The function is free to modify contents of JobMetadata in place (but the
// changes will be ignored unless JobUpdater is used).
type UpdateFn func(txn isql.Txn, md JobMetadata, ju *JobUpdater) error

type Updater struct {
	j   *Job
	txn isql.Txn
}

func (j *Job) NoTxn() Updater {
	return Updater{j: j}
}

func (j *Job) WithTxn(txn isql.Txn) Updater {
	return Updater{j: j, txn: txn}
}

func (j *Job) maybeWithTxn(txn isql.Txn) Updater {
	if txn != nil {
		return j.WithTxn(txn)
	}
	return j.NoTxn()
}

func (u Updater) update(ctx context.Context, updateFn UpdateFn) (retErr error) {
	if u.txn == nil {
		return u.j.registry.db.Txn(ctx, func(
			ctx context.Context, txn isql.Txn,
		) error {
			u.txn = txn
			return u.update(ctx, updateFn)
		})
	}
	ctx, sp := tracing.ChildSpan(ctx, "update-job")
	defer sp.Finish()

	var payload *jobspb.Payload
	var progress *jobspb.Progress
	var status Status
	var runStats *RunStats
	j := u.j
	defer func() {
		if retErr != nil {
			retErr = errors.Wrapf(retErr, "job %d", j.id)
			return
		}
		j.mu.Lock()
		defer j.mu.Unlock()
		if payload != nil {
			j.mu.payload = *payload
		}
		if progress != nil {
			j.mu.progress = *progress
		}
		if runStats != nil {
			j.mu.runStats = runStats
		}
		if status != "" {
			j.mu.status = status
		}
	}()

	const loadJobQuery = `
WITH
  latestpayload AS (
    SELECT job_id, value
    FROM system.job_info AS payload
    WHERE info_key = 'legacy_payload' AND job_id = $1
    ORDER BY written DESC LIMIT 1
  ),
  latestprogress AS (
    SELECT job_id, value
    FROM system.job_info AS progress
    WHERE info_key = 'legacy_progress' AND job_id = $1
    ORDER BY written DESC LIMIT 1
  )
SELECT status, payload.value AS payload, progress.value AS progress,
       claim_session_id, COALESCE(last_run, created), COALESCE(num_runs, 0)
FROM system.jobs AS j
INNER JOIN latestpayload AS payload ON j.id = payload.job_id
LEFT JOIN latestprogress AS progress ON j.id = progress.job_id
WHERE id = $1
`
	row, err := u.txn.QueryRowEx(
		ctx, "select-job", u.txn.KV(),
		sessiondata.NodeUserSessionDataOverride,
		loadJobQuery, j.ID(),
	)
	if err != nil {
		return err
	}
	if row == nil {
		return errors.Errorf("not found in system.jobs table")
	}

	if status, err = unmarshalStatus(row[0]); err != nil {
		return err
	}
	if payload, err = UnmarshalPayload(row[1]); err != nil {
		return err
	}
	if progress, err = UnmarshalProgress(row[2]); err != nil {
		return err
	}
	if j.session != nil {
		if row[3] == tree.DNull {
			return errors.Errorf(
				"with status %q: expected session %q but found NULL",
				status, j.session.ID())
		}
		storedSession := []byte(*row[3].(*tree.DBytes))
		if !bytes.Equal(storedSession, j.session.ID().UnsafeBytes()) {
			return errors.Errorf(
				"with status %q: expected session %q but found %q",
				status, j.session.ID(), sqlliveness.SessionID(storedSession))
		}
	} else {
		log.VInfof(ctx, 1, "job %d: update called with no session ID", j.ID())
	}

	lastRun, ok := row[4].(*tree.DTimestamp)
	if !ok {
		return errors.AssertionFailedf("expected timestamp last_run, but got %T", lastRun)
	}
	numRuns, ok := row[5].(*tree.DInt)
	if !ok {
		return errors.AssertionFailedf("expected int num_runs, but got %T", numRuns)
	}

	md := JobMetadata{
		ID:       j.ID(),
		Status:   status,
		Payload:  payload,
		Progress: progress,
		RunStats: &RunStats{
			NumRuns: int(*numRuns),
			LastRun: lastRun.Time,
		},
	}

	var ju JobUpdater
	if err := updateFn(u.txn, md, &ju); err != nil {
		return err
	}
	if j.registry.knobs.BeforeUpdate != nil {
		if err := j.registry.knobs.BeforeUpdate(md, ju.md); err != nil {
			return err
		}
	}

	if !ju.hasUpdates() {
		return nil
	}

	// Build a statement of the following form, depending on which properties
	// need updating:
	//
	//   UPDATE system.jobs
	//   SET
	//     [status = $2,]
	//     [payload = $y,]
	//     [progress = $z]
	//   WHERE
	//     id = $1

	var setters []string
	params := []interface{}{j.ID()} // $1 is always the job ID.
	addSetter := func(column string, value interface{}) {
		params = append(params, value)
		setters = append(setters, fmt.Sprintf("%s = $%d", column, len(params)))
	}

	if ju.md.Status != "" {
		addSetter("status", ju.md.Status)
	}
	if ju.md.RunStats != nil {
		runStats = ju.md.RunStats
		addSetter("last_run", ju.md.RunStats.LastRun)
		addSetter("num_runs", ju.md.RunStats.NumRuns)
	}

	var payloadBytes []byte
	if ju.md.Payload != nil {
		payload = ju.md.Payload
		var err error
		payloadBytes, err = protoutil.Marshal(payload)
		if err != nil {
			return err
		}
	}

	var progressBytes []byte
	if ju.md.Progress != nil {
		progress = ju.md.Progress
		progress.ModifiedMicros = timeutil.ToUnixMicros(u.now())
		var err error
		progressBytes, err = protoutil.Marshal(progress)
		if err != nil {
			return err
		}
	}

	if len(setters) != 0 {
		updateStmt := fmt.Sprintf(
			"UPDATE system.jobs SET %s WHERE id = $1",
			strings.Join(setters, ", "),
		)
		n, err := u.txn.ExecEx(
			ctx, "job-update", u.txn.KV(),
			sessiondata.NodeUserSessionDataOverride,
			updateStmt, params...,
		)
		if err != nil {
			return err
		}
		if n != 1 {
			return errors.Errorf(
				"expected exactly one row affected, but %d rows affected by job update", n,
			)
		}
	}

	// Insert the job payload and progress into the system.jobs_info table.
	infoStorage := j.InfoStorage(u.txn)
	if payloadBytes != nil {
		if err := infoStorage.WriteLegacyPayload(ctx, payloadBytes); err != nil {
			return err
		}
	}
	if progressBytes != nil {
		if err := infoStorage.WriteLegacyProgress(ctx, progressBytes); err != nil {
			return err
		}
	}

	return nil
}

// RunStats consists of job-run statistics: num of runs and last-run timestamp.
type RunStats struct {
	LastRun time.Time
	NumRuns int
}

// JobMetadata groups the job metadata values passed to UpdateFn.
type JobMetadata struct {
	ID       jobspb.JobID
	Status   Status
	Payload  *jobspb.Payload
	Progress *jobspb.Progress
	RunStats *RunStats
}

// CheckRunningOrReverting returns an InvalidStatusError if md.Status is not
// StatusRunning or StatusReverting.
func (md *JobMetadata) CheckRunningOrReverting() error {
	if md.Status != StatusRunning && md.Status != StatusReverting {
		return &InvalidStatusError{md.ID, md.Status, "update progress on", md.Payload.Error}
	}
	return nil
}

// JobUpdater accumulates changes to job metadata that are to be persisted.
type JobUpdater struct {
	md JobMetadata
}

// UpdateStatus sets a new status (to be persisted).
func (ju *JobUpdater) UpdateStatus(status Status) {
	ju.md.Status = status
}

// UpdatePayload sets a new Payload (to be persisted).
//
// WARNING: the payload can be large (resulting in a large KV for each version);
// it shouldn't be updated frequently.
func (ju *JobUpdater) UpdatePayload(payload *jobspb.Payload) {
	ju.md.Payload = payload
}

// UpdateProgress sets a new Progress (to be persisted).
func (ju *JobUpdater) UpdateProgress(progress *jobspb.Progress) {
	ju.md.Progress = progress
}

func (ju *JobUpdater) hasUpdates() bool {
	return ju.md != JobMetadata{}
}

// UpdateRunStats is used to update the exponential-backoff parameters last_run and
// num_runs in system.jobs table.
func (ju *JobUpdater) UpdateRunStats(numRuns int, lastRun time.Time) {
	ju.md.RunStats = &RunStats{
		NumRuns: numRuns,
		LastRun: lastRun,
	}
}

// UpdateHighwaterProgressed updates job updater progress with the new high water mark.
func UpdateHighwaterProgressed(highWater hlc.Timestamp, md JobMetadata, ju *JobUpdater) error {
	if err := md.CheckRunningOrReverting(); err != nil {
		return err
	}

	if highWater.Less(hlc.Timestamp{}) {
		return errors.Errorf("high-water %s is outside allowable range > 0.0", highWater)
	}
	md.Progress.Progress = &jobspb.Progress_HighWater{
		HighWater: &highWater,
	}
	ju.UpdateProgress(md.Progress)
	return nil
}

// Update is used to read the metadata for a job and potentially update it.
//
// The updateFn is called in the context of a transaction and is passed the
// current metadata for the job. It can choose to update parts of the metadata
// using the JobUpdater, causing them to be updated within the same transaction.
//
// Sample usage:
//
//	err := j.Update(ctx, func(_ *kv.Txn, md jobs.JobMetadata, ju *jobs.JobUpdater) error {
//	  if md.Status != StatusRunning {
//	    return errors.New("job no longer running")
//	  }
//	  ju.UpdateStatus(StatusPaused)
//	  // <modify md.Payload>
//	  ju.UpdatePayload(md.Payload)
//	}
//
// Note that there are various convenience wrappers (like FractionProgressed)
// defined in jobs.go.
func (u Updater) Update(ctx context.Context, updateFn UpdateFn) error {
	return u.update(ctx, updateFn)
}

func (u Updater) now() time.Time {
	return u.j.registry.clock.Now().GoTime()
}
