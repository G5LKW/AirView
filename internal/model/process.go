package model

import "strings"

// ProcessConfig ("process") holds process-level settings of the airview
// server: the log file and the soft memory limit (DESIGN §20.3: every
// setting file-configurable). They are applied when the server starts.
type ProcessConfig struct {
	// LogLevel is the lowest level written to the log: debug, info
	// (default), warn or error. AIRVIEW_DEBUG=1 still forces debug.
	LogLevel string `json:"logLevel,omitempty"`
	// LogMaxMB is the size (MiB) at which airview.log is rotated (1..4096;
	// 0 = automatic: 4 in standalone mode, 64 in aggregator mode).
	LogMaxMB int `json:"logMaxMb,omitempty"`
	// LogFiles is how many rotated files (airview.log.1, .2, ...) are kept
	// (1..100; 0 = automatic: 1 in standalone mode, 10 in aggregator mode).
	LogFiles int `json:"logFiles,omitempty"`
	// MemoryLimitMB is the soft Go heap limit in MiB (64..1048576; 0 =
	// automatic: 384 on Raspberry Pi class Linux devices in standalone
	// mode; in aggregator mode 85 % of a cgroup memory cap, else 80 % of
	// the physical memory; -1 = none). GOMEMLIMIT wins over it.
	MemoryLimitMB int `json:"memoryLimitMb,omitempty"`
	// JournalLevel is the lowest level copied to standard error, which
	// systemd stores in its journal: debug, info, warn or error ("" =
	// automatic: warn when started by systemd, so the SD card does not get
	// every line twice, else the same as logLevel). airview.log is not
	// affected.
	JournalLevel string `json:"journalLevel,omitempty"`
}

// Log levels of ProcessConfig.LogLevel.
const (
	LogDebug = "debug"
	LogInfo  = "info"
	LogWarn  = "warn"
	LogError = "error"
)

// Normalize clamps the values into their ranges and returns notes.
func (p *ProcessConfig) Normalize() []string {
	var n notes
	switch l := strings.ToLower(strings.TrimSpace(p.LogLevel)); l {
	case "", LogDebug, LogInfo, LogWarn, LogError:
		p.LogLevel = l
	default:
		n.addf("process.logLevel %q is not debug, info, warn or error; info is used", p.LogLevel)
		p.LogLevel = ""
	}
	switch l := strings.ToLower(strings.TrimSpace(p.JournalLevel)); l {
	case "", LogDebug, LogInfo, LogWarn, LogError:
		p.JournalLevel = l
	default:
		n.addf("process.journalLevel %q is not debug, info, warn or error; automatic is used", p.JournalLevel)
		p.JournalLevel = ""
	}
	zeroOrRangeI(&n, "process.logMaxMb", &p.LogMaxMB, 1, 4096)
	zeroOrRangeI(&n, "process.logFiles", &p.LogFiles, 1, 100)
	if p.MemoryLimitMB != -1 {
		zeroOrRangeI(&n, "process.memoryLimitMb", &p.MemoryLimitMB, 64, 1<<20)
	}
	return n
}

// EffectiveJournalLevel returns the lowest level copied to standard error:
// the configured JournalLevel, else warn under systemd (underSystemd: the
// JOURNAL_STREAM variable is set), else logLevel ("" = info).
func (p ProcessConfig) EffectiveJournalLevel(underSystemd bool) string {
	if p.JournalLevel != "" {
		return p.JournalLevel
	}
	if underSystemd {
		return LogWarn
	}
	if p.LogLevel != "" {
		return p.LogLevel
	}
	return LogInfo
}

// LogLimits returns the effective log size cap (bytes) and rotated-file
// count for the mode.
func (p ProcessConfig) LogLimits(aggregator bool) (maxBytes int64, files int) {
	mb, files := p.LogMaxMB, p.LogFiles
	if mb <= 0 {
		mb = 4
		if aggregator {
			mb = 64
		}
	}
	if files <= 0 {
		files = 1
		if aggregator {
			files = 10
		}
	}
	return int64(mb) << 20, files
}
