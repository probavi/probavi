package main

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
)

// chainrestore.go runs what chain.go decided.
//
// Selection and restore are separated the way the bak_dir work separated
// them: every candidate is probed through one scratch path that the next
// probe overwrites, so the sandbox never holds the whole directory, and
// only the artifacts the chain actually restores are transferred to keep.
// Probing is how the drill finds the backups; the transfers that follow
// are the recovery it measures, and both are reported that way.

// chainSelection is a built chain with the artifacts still on the host.
type chainSelection struct {
	nodes    []chainNode
	database string
}

// selectChain probes every candidate in the directory and assembles the
// restore chain for one database.
func selectChain(ctx context.Context, c *core, plan *sourcePlan, probePath string) (*chainSelection, *protoError) {
	var nodes []chainNode
	for _, candidate := range plan.candidates {
		if _, perr := c.putFile(ctx, putFileArgs{SourcePath: candidate, DestPath: probePath, Mode: "0600"}); perr != nil {
			return nil, perr
		}
		sets, perr := probeBackupSets(ctx, c, probePath)
		if perr != nil {
			// Backup media the engine cannot read fails the drill: falling
			// back would build a chain from whatever happened to parse.
			return nil, perr
		}
		for _, set := range sets {
			nodes = append(nodes, chainNode{hostPath: candidate, set: set})
		}
	}
	if len(nodes) == 0 {
		return nil, noFullBackup(plan, nil)
	}

	database, perr := chooseDatabase(nodes, plan.databaseName)
	if perr != nil {
		return nil, perr
	}
	chain, perr := buildChain(nodesFor(nodes, database))
	if perr != nil {
		return nil, perr
	}
	return &chainSelection{nodes: chain, database: database}, nil
}

// chooseDatabase decides whose backups the chain is built from. One
// database in the directory needs no saying; more than one is an
// ambiguity the drill config has to settle, because picking for the
// operator would mean a record naming a database nobody chose.
func chooseDatabase(nodes []chainNode, requested string) (string, *protoError) {
	present := databasesIn(nodes)
	if requested != "" {
		for _, name := range present {
			if name == requested {
				return name, nil
			}
		}
		return "", protoErr("source_not_found", false,
			"the directory holds no backup of database %s: it holds %s",
			requested, nameList(present, 5))
	}
	switch len(present) {
	case 0:
		return "", protoErr("source_corrupt", false,
			"the backup headers name no database, so there is no chain to build")
	case 1:
		return present[0], nil
	default:
		return "", protoErr("invalid_request", false,
			"the directory holds backups of several databases (%s): name the one to restore "+
				"in source.params.database_name", nameList(present, 5))
	}
}

// restoreChain replays the chain in order: every member with NORECOVERY
// and the last one with RECOVERY, which is what makes the database usable
// exactly once, at the end. The first member is the full backup and needs
// the server-side MOVEs; the rest land on the files it created.
func restoreChain(ctx context.Context, c *core, sel *chainSelection, database, scratch string,
	logger *slog.Logger) (transfer, restore float64, perr *protoError) {
	for i, node := range sel.nodes {
		path := fmt.Sprintf("%s/probavi-chain-%02d.bak", scratch, i)
		put, perr := c.putFile(ctx, putFileArgs{SourcePath: node.hostPath, DestPath: path, Mode: "0600"})
		if perr != nil {
			return 0, 0, perr
		}
		transfer += put.DurationSeconds

		last := i == len(sel.nodes)-1
		val, stderr, perr := execChainRestore(ctx, c, path, database, node.set, last, i == 0)
		if perr != nil {
			return 0, 0, perr
		}
		if val.ExitCode != 0 {
			return 0, 0, mapChainFailure(node, stderr)
		}
		restore += val.DurationSeconds
		logger.Info("chain member restored", "member", node.name(),
			"type", backupTypeName(node.set.backupType), "recovered", last)
	}
	return transfer, restore, nil
}

// execChainRestore runs one member. Everything operator-supplied was
// validated before it could reach T-SQL: the database name against
// databasePattern, the backup set number parsed out of the header.
func execChainRestore(ctx context.Context, c *core, path, database string, set backupSet,
	withRecovery, withMoves bool) (*execValue, []byte, *protoError) {
	verb := "DATABASE"
	if set.backupType == backupTypeLog {
		verb = "LOG"
	}
	recovery := "NORECOVERY"
	if withRecovery {
		recovery = "RECOVERY"
	}
	moves := "0"
	if withMoves {
		moves = "1"
	}
	val, _, stderr, perr := c.exec(ctx, execArgs{
		Argv: []string{"sh", "-c", chainRestoreScript, "sh", path, database,
			strconv.Itoa(set.position), verb, recovery, moves},
		Env: sqlcmdEnv(),
	})
	if perr != nil {
		return nil, nil, perr
	}
	return val, stderr, nil
}

// mapChainFailure names the member that failed. The engine's own message
// is the useful part — a broken chain reports the log sequence number it
// needed (measured: Msg 4305) — so it is passed through rather than
// summarised away.
func mapChainFailure(node chainNode, stderr []byte) *protoError {
	line := verdictLine(stderr)
	for _, marker := range []string{"incorrectly formed", "is empty", "not a valid"} {
		if strings.Contains(line, marker) {
			return protoErr("source_corrupt", false,
				"sql server rejected %s: %s", node.name(), line)
		}
	}
	return protoErr("restore_failed", false,
		"restoring %s (%s) failed: %s", node.name(), backupTypeName(node.set.backupType), line)
}

// provisionChain restores a whole backup chain and reports it as one
// recovery: the transfers and the restores of every member count, because
// unlike the probing that finds them, all of them are the recovery path
// this drill measures.
func provisionChain(ctx context.Context, c *core, plan *sourcePlan, database, scratch string,
	readySeconds float64, logger *slog.Logger) (any, *protoError) {
	sel, perr := selectChain(ctx, c, plan, scratch+"/probavi-chain-probe.bak")
	if perr != nil {
		return nil, perr
	}
	logger.Info("chain built", "database", sel.database, "members", len(sel.nodes),
		"order", chainNames(sel.nodes))

	// The full backup opens the chain; its header names the origin server.
	if perr := checkEngineVersion(ctx, c, sel.nodes[0].set.softwareMajor); perr != nil {
		return nil, perr
	}

	src, perr := plan.chainIdentity(sel, plan.loc)
	if perr != nil {
		return nil, perr
	}
	transfer, restore, perr := restoreChain(ctx, c, sel, database, scratch, logger)
	if perr != nil {
		return nil, perr
	}
	logger.Info("chain restored", "seconds", restore)

	state := map[string]any{
		"database":     database,
		"chain":        chainNames(sel.nodes),
		"chain_length": strconv.Itoa(len(sel.nodes)),
	}
	return provisionResult(database, src, state, timings{
		engineReady: readySeconds,
		transfer:    transfer,
		restore:     restore,
	}), nil
}
