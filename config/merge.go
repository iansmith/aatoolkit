package config

import (
	"fmt"
	"maps"
	"slices"
)

// mergeOverlay deep-merges the local overlay onto the committed base config.
//
//   - [supervisor]: scalar fields from local win when set (non-zero).
//   - [[server]]: matched by name. Scalars from local win when set (non-zero);
//     env maps merge per-key (local keys win on collision, base keys not
//     present in local are kept). A local server name absent from base is a
//     hard error — the local overlay may only override, never introduce new
//     servers.
func mergeOverlay(base, local Config) (Config, error) {
	merged := base
	merged.Supervisor = mergeSupervisor(base.Supervisor, local.Supervisor)

	baseByName := make(map[string]int, len(base.Servers))
	for i, s := range base.Servers {
		baseByName[s.Name] = i
	}

	mergedServers := slices.Clone(base.Servers)

	for _, ls := range local.Servers {
		idx, ok := baseByName[ls.Name]
		if !ok {
			return Config{}, fmt.Errorf(
				"local overlay: server %q not present in committed config", ls.Name)
		}
		mergedServers[idx] = mergeServer(mergedServers[idx], ls)
	}

	merged.Servers = mergedServers
	return merged, nil
}

func mergeSupervisor(base, local Supervisor) Supervisor {
	merged := base
	if local.BaseDir != "" {
		merged.BaseDir = local.BaseDir
	}
	if local.LogDir != "" {
		merged.LogDir = local.LogDir
	}
	if local.LockFile != "" {
		merged.LockFile = local.LockFile
	}
	if local.GracePeriod.Duration != 0 {
		merged.GracePeriod = local.GracePeriod
	}
	if local.ReadyTimeout.Duration != 0 {
		merged.ReadyTimeout = local.ReadyTimeout
	}
	if local.PollInterval.Duration != 0 {
		merged.PollInterval = local.PollInterval
	}
	if local.HealthTimeout.Duration != 0 {
		merged.HealthTimeout = local.HealthTimeout
	}
	return merged
}

// mergeServer overlays local's set fields onto base, per the rules
// documented on mergeOverlay. "enabled" is deliberately excluded: it's a
// bool, so it always decodes to a concrete value from TOML, and we can't
// distinguish "false" from "absent" with plain bool decoding — the
// committed file remains the sole source of truth for desired state.
func mergeServer(base, local Server) Server {
	merged := mergeServerIdentity(base, local)
	merged = mergeServerLaunch(merged, local)
	merged.Env = mergeEnv(base.Env, local.Env)
	return merged
}

// mergeServerIdentity overlays the fields common to every server type
// (identity, network shape, health, and per-server timeout overrides).
func mergeServerIdentity(base, local Server) Server {
	merged := base
	if local.Type != "" {
		merged.Type = local.Type
	}
	if local.Host != "" {
		merged.Host = local.Host
	}
	if local.Port != 0 {
		merged.Port = local.Port
	}
	if len(local.Listens) != 0 {
		merged.Listens = local.Listens
	}
	merged.Health = mergeHealth(base.Health, local.Health)
	if local.GracePeriod.Duration != 0 {
		merged.GracePeriod = local.GracePeriod
	}
	if local.ReadyTimeout.Duration != 0 {
		merged.ReadyTimeout = local.ReadyTimeout
	}
	return merged
}

// mergeServerLaunch overlays the fields specific to how a server is
// launched (mlx/python/source/exec — only the fields for the server's own
// type are ever populated in practice, but overlaying all of them is safe
// since unused fields stay zero-valued).
func mergeServerLaunch(base, local Server) Server {
	merged := base
	if local.Model != "" {
		merged.Model = local.Model
	}
	if local.Venv != "" {
		merged.Venv = local.Venv
	}
	if local.Entry != "" {
		merged.Entry = local.Entry
	}
	if len(local.Packages) != 0 {
		merged.Packages = local.Packages
	}
	if local.Build != "" {
		merged.Build = local.Build
	}
	if local.Binary != "" {
		merged.Binary = local.Binary
	}
	if local.Dir != "" {
		merged.Dir = local.Dir
	}
	if local.Command != "" {
		merged.Command = local.Command
	}
	if len(local.Args) != 0 {
		merged.Args = local.Args
	}
	return merged
}

// mergeHealth overlays local's set health fields onto base, field by field —
// consistent with the scalar local-wins rule. An unset local field (empty
// host/path, zero port) leaves base's value intact, so a local override that
// touches only one field (e.g. path) does not wipe the others.
func mergeHealth(base, local Health) Health {
	merged := base
	if local.Host != "" {
		merged.Host = local.Host
	}
	if local.Port != 0 {
		merged.Port = local.Port
	}
	if local.Path != "" {
		merged.Path = local.Path
	}
	return merged
}

// mergeEnv merges local's keys onto base's, per-key, with local winning on
// collision.
func mergeEnv(base, local map[string]string) map[string]string {
	if len(base) == 0 && len(local) == 0 {
		return nil
	}
	merged := make(map[string]string, len(base)+len(local))
	maps.Copy(merged, base)
	maps.Copy(merged, local)
	return merged
}
