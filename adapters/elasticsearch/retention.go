package main

import (
	"context"
	"encoding/json"
	"time"
)

// retention.go stops the sandbox from running the backup's own
// lifecycle policies against the artifact it is proving.
//
// Elasticsearch has two machineries that delete data on a schedule, and
// a restored snapshot re-arms both:
//
//   - Index Lifecycle Management. A restored index keeps its
//     `index.lifecycle.name` setting (it should — a check reading the
//     settings is entitled to see what the operator declared), and a
//     fresh node is not empty of policies: it ships 47 built-in ones
//     (measured on 8.19 and 9.5), `7-days-default` through
//     `365-days-default` and the stack's `logs@lifecycle` among them.
//     An index that names one of those is managed the moment it lands.
//   - Data stream lifecycle. A data stream's retention travels inside
//     the data stream's own metadata, which the restore brings along
//     with its backing indices — no policy lookup is needed at all.
//
// Both measure age from what the artifact carries (rollover instants,
// `index.lifecycle.origination_date`), so a backup older than its own
// retention is past due the moment it is restored. Measured, with
// polling accelerated: a data stream under `7-days-default` and one
// under a one-day retention, each with a rolled-over generation past
// its age, restored with 4/4 shards reported successful — the data
// stream lifecycle deleted its older generation **eight seconds** after
// the restore ("due to the lapsed [1d] retention period", in the
// node's own words), ILM deleted the other twenty seconds later. Five
// documents became two while the restore stood as a success.
//
// # Two switches, both verified, neither a rewrite
//
// ILM has a switch: `POST _ilm/stop`, verified through `GET _ilm/status`
// reading STOPPED (the stop is asynchronous and reads STOPPING first,
// measured). Data stream lifecycle has none, so it is held off by its
// poll interval — `data_streams.lifecycle.poll_interval`, default five
// minutes — pinned to a hundred years as a launch setting, before the
// node exists to poll anything, and verified by reading the setting
// back through the cluster settings API. Neither touches the artifact:
// the index setting and the data stream's retention stay exactly as the
// backup recorded them, and only execution is suspended, for the life
// of the sandbox.
//
// Measured again with both pins in place and ILM polling forced to one
// second: every generation and every document survived, and the
// retention still read `1d` on the data stream.
//
// The restore also leaves the cluster state in the artifact
// (`include_global_state` stays false): restoring it would bring the
// backup's own persistent settings, which could override the poll
// interval pinned here — and the policies it would add have nothing to
// run on while ILM is stopped.

const (
	// lifecyclePollInterval is a hundred years, which is how the data
	// stream lifecycle is kept from ever polling in a sandbox whose life
	// is minutes: it has no stop switch (measured: no such setting
	// exists in either verified line), and the setting accepts the value
	// (measured).
	lifecyclePollInterval = "876000h"

	// lifecyclePollSetting is the cluster setting the launch pins;
	// passed as a node setting it shows up under `defaults` when the
	// cluster settings are read with include_defaults (measured).
	lifecyclePollSetting = "data_streams.lifecycle.poll_interval"

	// ilmStopped is the operation mode that means no lifecycle action
	// runs.
	ilmStopped = "STOPPED"

	ilmStopBudget = 30 * time.Second
	ilmStopPoll   = time.Second
)

// pinLifecycle makes sure the sandbox will not run the backup's lifecycle
// policies, and returns what it measured. It runs after readiness and
// before the repository is registered: nothing the artifact carries has
// landed yet, so nothing can have run.
//
// Both verifications are positive-evidence gates: an answer in the
// API's shape is judged; anything else — the conformance suite's
// simulated sandbox answers every exec with a bare `1` — is left to the
// restore's own gates.
func pinLifecycle(ctx context.Context, c *core) (float64, *protoError) {
	total, perr := stopILM(ctx, c)
	if perr != nil {
		return 0, perr
	}
	seconds, perr := verifyLifecyclePoll(ctx, c)
	if perr != nil {
		return 0, perr
	}
	return total + seconds, nil
}

// stopILM issues the stop and waits for the node to say STOPPED.
func stopILM(ctx context.Context, c *core) (float64, *protoError) {
	val, _, stderr, perr := c.exec(ctx, execArgs{Argv: []string{"curl", "-s", "-XPOST",
		serverURL + "/_ilm/stop"}})
	if perr != nil {
		return 0, perr
	}
	if val.ExitCode != 0 {
		return 0, protoErr("restore_failed", false,
			"the node stopped answering while suspending index lifecycle management: %s",
			firstLine(stderr))
	}
	total := val.DurationSeconds
	begin := time.Now()
	for {
		val, stdout, _, perr := c.exec(ctx, execArgs{Argv: []string{"curl", "-s",
			serverURL + "/_ilm/status"}})
		if perr != nil {
			return 0, perr
		}
		total += val.DurationSeconds
		mode, answered := ilmMode(stdout)
		if !answered || mode == ilmStopped {
			return total, nil
		}
		if time.Since(begin) > ilmStopBudget {
			return 0, refusedLifecycle("index lifecycle management still reports " + mode +
				" " + ilmStopBudget.String() + " after the stop was acknowledged")
		}
		select {
		case <-ctx.Done():
			return 0, protoErr("cancelled", true, "cancelled while suspending index lifecycle management")
		case <-time.After(ilmStopPoll):
		}
	}
}

// ilmMode reads `GET _ilm/status`; answered is false for anything that is
// not the API's shape.
func ilmMode(stdout []byte) (string, bool) {
	status := struct {
		OperationMode string `json:"operation_mode"`
	}{}
	if err := json.Unmarshal(stdout, &status); err != nil || status.OperationMode == "" {
		return "", false
	}
	return status.OperationMode, true
}

// verifyLifecyclePoll reads the data stream lifecycle poll interval back
// and refuses a node that did not take the launch setting.
func verifyLifecyclePoll(ctx context.Context, c *core) (float64, *protoError) {
	val, stdout, _, perr := c.exec(ctx, execArgs{Argv: []string{"curl", "-s",
		serverURL + "/_cluster/settings?include_defaults=true&flat_settings=true"}})
	if perr != nil {
		return 0, perr
	}
	interval, answered := lifecyclePollValue(stdout)
	if !answered {
		return val.DurationSeconds, nil
	}
	if interval != lifecyclePollInterval {
		return 0, refusedLifecycle("the data stream lifecycle poll interval reads " + interval +
			" where the launch pinned " + lifecyclePollInterval)
	}
	return val.DurationSeconds, nil
}

// lifecyclePollValue reads the effective poll interval out of the flat
// cluster settings: a persistent or transient value wins over the node
// default, exactly as the engine resolves it.
func lifecyclePollValue(stdout []byte) (string, bool) {
	settings := struct {
		Persistent map[string]string `json:"persistent"`
		Transient  map[string]string `json:"transient"`
		Defaults   map[string]string `json:"defaults"`
	}{}
	if err := json.Unmarshal(stdout, &settings); err != nil {
		return "", false
	}
	for _, scope := range []map[string]string{settings.Transient, settings.Persistent, settings.Defaults} {
		if v, ok := scope[lifecyclePollSetting]; ok {
			return v, true
		}
	}
	return "", false
}

func refusedLifecycle(reason string) *protoError {
	return protoErr("invalid_request", false,
		"the sandbox engine would not suspend its lifecycle policies (%s) — a restored index "+
			"keeps its lifecycle settings and a restored data stream its retention, a fresh node "+
			"ships dozens of built-in policies for them to name, and a backup older than its own "+
			"retention loses generations seconds after the restore (measured), so the drill would "+
			"prove whatever survived the clock rather than what the backup holds", reason)
}
