// Package jobs is the background job queue of the optional job module
// (JOBS_ENABLED=true).
//
// A handler of the API enqueues a job with Queue.Enqueue: one row in the MySQL
// table jobs, written with the database clock. A Pool on every replica claims
// the due rows of the types that its process registered (a transaction with
// SELECT ... FOR UPDATE SKIP LOCKED), runs the Handler in one of its worker
// goroutines, and records the outcome:
//
//   - the handler returns nil: succeeded;
//   - it returns an error: the job runs again after an exponential backoff, or
//     is dead when its attempts are used up;
//   - it returns Permanent(err): dead at once;
//   - the process stops: the job is released, and the attempt is not counted.
//
// While a handler runs, the pool extends the lease of the row every third of
// the visibility timeout. A row whose lease passed (the process crashed or
// hung) is claimed again by any replica, and goes to dead instead when it used
// all its attempts. Every update of a running job is fenced by the worker ID,
// so a worker that lost its job drops its result.
//
// Delivery is at least once: a job can run twice (a lease that passed while a
// hung handler still ran, a result that could not be written). Handlers must
// be idempotent. There is no deduplication.
package jobs
