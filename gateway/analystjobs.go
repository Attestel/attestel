package main

// analystjobs.go — the ASYNC half of the analyst lane.
//
// WHY THIS FILE EXISTS. `POST /api/analyst/{ticker}` used to run the pipeline INSIDE the request
// and answer with the finished envelope. Up to eight sequential local-model generations do not fit
// inside an HTTP request: `deploy/nginx.conf.template` closes the connection at
// `proxy_read_timeout 120s` and the gateway's own client gave up at 130 s (`main.go`), so in
// production the flagship flow returned 504 and the card fell back to "Not yet assessed" — the run
// itself kept going, unobserved, and the user was told nothing. A run that outlives its request is
// not an outage to paper over with a bigger timeout; it is a job, and this file makes it one.
//
// THE SHAPE. POST validates, then starts ONE background run and answers `202` immediately with a
// `runId`. `GET /api/analyst/runs/{runID}` reports `running | done | failed` and carries the
// envelope once there is one. The browser polls that read, so a slow model costs the proxy nothing
// and a page reload or a navigation loses no work.
//
// INVARIANT #4 IS UNCHANGED, AND THIS FILE IS WHERE IT WOULD BE BROKEN FIRST.
//   * The POST is still the ONLY thing that can cause a model generation. It is still
//     user-initiated, still refuses non-POST, still takes the cross-process model lease downstream.
//   * `GET /api/analyst/runs/{id}` NEVER starts, resumes, retries or extends a run. It is a
//     dictionary lookup over a map this process already holds. That is precisely why the status
//     read may be polled and the run route may not, and `analyst_test.go` asserts a status GET on
//     an unknown id reaches no upstream at all.
//   * A second POST with the same (ticker, horizon, asOf, uid) while a run is in flight ATTACHES to
//     that run and returns its id. It does not start a second one — two concurrent runs would sit
//     in the same `{READS_DIR}/.model.lock` queue and starve the interactive path, which is the
//     failure invariant #4 exists to prevent.
//
// Stdlib only (invariant #5).

import (
	"context"
	"encoding/json"
	"hash/fnv"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// Run states. Three, and no fourth: a run is in flight, it produced an envelope, or it did not.
// "failed" is a RESULT — the client says so out loud rather than reverting to the pre-run state,
// which is the specific dishonesty this lane was reported for.
const (
	analystRunning = "running"
	analystDone    = "done"
	analystFailed  = "failed"
)

// analystRunRetention is how long a FINISHED job stays readable after it ends. It only has to
// outlive the client's poll loop and a reload or two; the envelope itself lives in `s.cache` under
// `AnalystTTL`, so nothing is lost when a record is reaped.
const analystRunRetention = 30 * time.Minute

// analystRunPollMs is the cadence the server suggests to the client. Advisory: the client honours
// it, the server does not depend on it, and no server-side timer reads it (invariant #4).
const analystRunPollMs = 3000

// analystJob is one run. It is written by exactly one goroutine — the one that runs it — and read
// under the store's lock, which hands out copies (`snapshot`) so a reader never shares memory with
// the writer.
type analystJob struct {
	ID        string
	Key       string
	Ticker    string
	Horizon   string
	AsOf      string
	Status    string
	StartedAt time.Time
	EndedAt   time.Time
	Result    map[string]any
	Err       string
}

func (j *analystJob) snapshot() analystJob { return *j }

// analystJobs is the in-memory run registry. In-memory is the right scope: a run is bounded by
// `analystTimeout`, the envelope is cached separately, and a gateway restart during a run is
// reported honestly to the client as an unknown run rather than resurrected.
type analystJobs struct {
	mu   sync.Mutex
	byID map[string]*analystJob
}

func newAnalystJobs() *analystJobs { return &analystJobs{byID: map[string]*analystJob{}} }

// analystRuns lazily builds the registry. Lazily, because every test in this package builds a
// `&Server{cfg: …, cache: …, http: …}` literal by hand and none of them should have to learn about
// a new field to keep compiling.
var analystJobsInit sync.Mutex

func (s *Server) analystRuns() *analystJobs {
	analystJobsInit.Lock()
	defer analystJobsInit.Unlock()
	if s.jobs == nil {
		s.jobs = newAnalystJobs()
	}
	return s.jobs
}

// analystRunID derives the run id from the CACHE KEY, so the same (ticker, horizon, asOf, uid) is
// always the same run id. That is what lets a second POST attach to an in-flight run instead of
// starting a rival one, and it keeps §1's live/historical isolation: the key already separates
// them, so the ids cannot collide either.
func analystRunID(key string) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte(key))
	return "analyst_" + strconv.FormatUint(h.Sum64(), 16)
}

// begin registers a run for `key` unless one is already in flight for it.
//
// Returns the job as it now stands and whether THIS caller owns it. `owned == false` means an
// identical run was already going: the caller attaches to it and starts nothing.
func (js *analystJobs) begin(key, ticker, horizon, asOf string) (analystJob, bool) {
	js.mu.Lock()
	defer js.mu.Unlock()
	js.sweepLocked(time.Now())

	id := analystRunID(key)
	if existing, ok := js.byID[id]; ok && existing.Status == analystRunning {
		return existing.snapshot(), false
	}
	job := &analystJob{
		ID: id, Key: key, Ticker: ticker, Horizon: horizon, AsOf: asOf,
		Status: analystRunning, StartedAt: time.Now(),
	}
	js.byID[id] = job
	return job.snapshot(), true
}

// finish records a terminal state. `err` non-empty means the run failed; both are recorded, and
// neither is silently dropped.
func (js *analystJobs) finish(id string, result map[string]any, errText string) {
	js.mu.Lock()
	defer js.mu.Unlock()
	job, ok := js.byID[id]
	if !ok {
		return
	}
	job.EndedAt = time.Now()
	job.Result = result
	job.Err = errText
	if errText != "" {
		job.Status = analystFailed
	} else {
		job.Status = analystDone
	}
}

// get returns a copy of one job.
func (js *analystJobs) get(id string) (analystJob, bool) {
	js.mu.Lock()
	defer js.mu.Unlock()
	js.sweepLocked(time.Now())
	job, ok := js.byID[id]
	if !ok {
		return analystJob{}, false
	}
	return job.snapshot(), true
}

// sweepLocked drops finished records past their retention, and running records that outlived the
// hard bound on a run by a wide margin (a goroutine that cannot report its own end is a bug, but a
// map that grows forever because of one is a worse one).
func (js *analystJobs) sweepLocked(now time.Time) {
	for id, job := range js.byID {
		switch job.Status {
		case analystRunning:
			if now.Sub(job.StartedAt) > analystTimeout+5*time.Minute {
				delete(js.byID, id)
			}
		default:
			if now.Sub(job.EndedAt) > analystRunRetention {
				delete(js.byID, id)
			}
		}
	}
}

// ───────────────────────────────────────────────────────────────────────────────── the runner

// startAnalystRun launches the background run for an already-registered job.
//
// The context is `context.Background()` with `analystTimeout`, NOT the request context: the whole
// point is that the run outlives the POST that asked for it. The identity and every validated
// parameter are captured by value before the goroutine starts, so nothing here reads a
// `*http.Request` after its handler returned.
func (s *Server) startAnalystRun(job analystJob, id identity) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), analystTimeout)
		defer cancel()

		envelope := analystContextProvider(s, ctx, job.Ticker, job.Horizon, job.AsOf, id)
		payload := map[string]any{"ticker": job.Ticker, "context": envelope}
		if job.Horizon != "" {
			payload["horizon"] = job.Horizon
		}
		if job.AsOf != "" {
			payload["asOf"] = job.AsOf
		}

		res, err := s.postAnalyst(ctx, s.cfg.LLMURL+"/analyst", payload)
		if err != nil {
			// OFFLINE PATH, unchanged in meaning from the synchronous version: `services/llm`'s own
			// model-down path returns deterministic stubs rather than errors, so reaching here means
			// the SERVICE is gone (or the run hit `analystTimeout`). Nothing is cached, so the next
			// POST retries — but the failure is now RECORDED and served, instead of vanishing with
			// the request that carried it.
			s.analystRuns().finish(job.ID, map[string]any{
				"ticker": job.Ticker, "available": false, "degraded": true,
				"asOf": job.AsOf, "horizon": job.Horizon, "error": err.Error(),
				"disclaimer": analystDisclaimer,
			}, err.Error())
			return
		}

		res["available"] = true
		// §9.18, DISCLOSURE_CLASSIFICATION rule 3: the disclosure travels with the output it
		// qualifies. `analyst.py` stamps it; this is the belt to that braces and is the ONLY value
		// in the response the gateway supplies itself.
		if _, ok := res["disclaimer"]; !ok {
			res["disclaimer"] = analystDisclaimer
		}
		if buf, mErr := json.Marshal(res); mErr == nil {
			s.cache.set(job.Key, buf, s.cfg.AnalystTTL())
		}
		s.analystRuns().finish(job.ID, res, "")
	}()
}

// postAnalyst is `postJSON` on a client with NO overall timeout, bounded by the context instead.
//
// `main.go` gives the shared client `Timeout: 130 * time.Second`, which is right for every other
// upstream in this service and fatal for this one: a `http.Client.Timeout` caps the whole exchange
// regardless of the context, so a run longer than 130 s died at the client even after the proxy
// stopped being the limit. The single bound on a run is `analystTimeout` (10 min), applied by the
// caller's context.
func (s *Server) postAnalyst(ctx context.Context, url string, payload any) (map[string]any, error) {
	long := *s.http
	long.Timeout = 0
	return postJSONWith(ctx, &long, url, payload)
}

// ────────────────────────────────────────────────────────────────────────────── the status read

// handleAnalystRun serves one run's status. IT STARTS NOTHING. See the invariant #4 block above:
// this is the route the browser is allowed to poll precisely because reaching it cannot cause a
// model generation, and it touches no upstream at all.
func (s *Server) handleAnalystRun(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("runID")
	if runID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "missing run id"})
		return
	}
	job, ok := s.analystRuns().get(runID)
	if !ok {
		// An unknown id is a real answer: the run was reaped, or this process did not start it (a
		// restart, or another replica). The client says so and offers a fresh run — it must never
		// re-POST on its own, which would make a page load capable of starting one.
		writeJSON(w, http.StatusNotFound, map[string]any{
			"error":      "no such analyst run: it finished long ago, or the gateway restarted",
			"code":       "unknown_run",
			"runId":      runID,
			"disclaimer": analystDisclaimer,
		})
		return
	}
	writeJSON(w, http.StatusOK, analystRunBody(job))
}

// analystRunBody is the wire shape of a run, in every state. The envelope is spread at the top
// level when there is one, so a finished run reads exactly like the old synchronous response plus
// `status` and `runId` — the client's rendering path did not have to learn a second shape.
func analystRunBody(job analystJob) map[string]any {
	body := map[string]any{}
	for k, v := range job.Result {
		body[k] = v
	}
	body["runId"] = job.ID
	body["status"] = job.Status
	body["ticker"] = job.Ticker
	body["horizon"] = job.Horizon
	body["asOf"] = job.AsOf
	body["startedAt"] = job.StartedAt.UTC().Format(time.RFC3339)
	body["pollAfterMs"] = analystRunPollMs
	if job.Status == analystRunning {
		body["elapsedMs"] = time.Since(job.StartedAt).Milliseconds()
	} else {
		body["elapsedMs"] = job.EndedAt.Sub(job.StartedAt).Milliseconds()
		body["finishedAt"] = job.EndedAt.UTC().Format(time.RFC3339)
	}
	if job.Err != "" {
		body["error"] = job.Err
	}
	if _, ok := body["disclaimer"]; !ok {
		body["disclaimer"] = analystDisclaimer
	}
	return body
}
