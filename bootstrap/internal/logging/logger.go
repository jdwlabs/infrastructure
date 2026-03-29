package logging

import (
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/buffer"
	"go.uber.org/zap/zapcore"

	"github.com/jdwlabs/infrastructure/bootstrap/internal/types"
)

// ANSI color codes
const (
	colorReset     = "\033[0m"
	colorDim       = "\033[2m"
	colorRed       = "\033[31m"
	colorGreen     = "\033[32m"
	colorYellow    = "\033[33m"
	colorBlue      = "\033[34m"
	colorWhiteOnRd = "\033[37;41m" // white text on red background
)

// RunSession manages a single bootstrap run's log files and lifecycle
type RunSession struct {
	RunDir      string
	StartTime   time.Time
	Logger      *zap.Logger
	AuditLog    *AuditLogger
	Console     io.Writer // tees to stderr + console.log; use for banner/box output
	ConsoleFile io.Writer // console.log only; use for recording input without echoing
	NoColor     bool
	Config      *types.Config
	closers     []io.Closer
	runsLogDir  string

	// Operational counters for SUMMARY.txt (set by caller during execution)
	ControlPlanes   int
	Workers         int
	AddedNodes      int
	RemovedNodes    int
	UpdatedConfigs  int
	BootstrapNeeded bool
}

// NewRunSession creates a timestamped run directory, opens log files,
// and builds a tee'd zap.Logger writing to stderr + console.log + structured.log.
func NewRunSession(cfg *types.Config) (*RunSession, error) {
	now := time.Now()
	dateDir := now.Format("2006-01-02")
	runName := "run-" + now.Format("20060102_150405")
	runDir := filepath.Join(cfg.LogDir, dateDir, runName)

	if err := os.MkdirAll(runDir, 0755); err != nil {
		return nil, fmt.Errorf("create run directory %s: %w", runDir, err)
	}

	// Open log files
	consoleFile, err := os.Create(filepath.Join(runDir, "console.log"))
	if err != nil {
		return nil, fmt.Errorf("create console.log: %w", err)
	}

	structuredFile, err := os.Create(filepath.Join(runDir, "structured.log"))
	if err != nil {
		consoleFile.Close()
		return nil, fmt.Errorf("create structured.log: %w", err)
	}

	auditFile, err := os.Create(filepath.Join(runDir, "audit.log"))
	if err != nil {
		consoleFile.Close()
		structuredFile.Close()
		return nil, fmt.Errorf("create audit.log: %w", err)
	}

	// Parse log level
	level := parseZapLevel(cfg.LogLevel)

	// Build tee core
	teeCore := buildTeeCore(level, cfg.NoColor, consoleFile, structuredFile)

	logger := zap.New(teeCore, zap.AddStacktrace(zap.FatalLevel))

	session := &RunSession{
		RunDir:      runDir,
		StartTime:   now,
		Logger:      logger,
		AuditLog:    NewAuditLogger(auditFile),
		Console:     io.MultiWriter(os.Stderr, consoleFile),
		ConsoleFile: consoleFile,
		NoColor:     cfg.NoColor,
		Config:      cfg,
		closers:     []io.Closer{consoleFile, structuredFile, auditFile},
		runsLogDir:  cfg.LogDir,
	}

	// Write to runs.log registry
	session.registerRun()

	// Update latest.txt symlink
	session.updateLatest()

	// Write session header
	session.writeHeader()

	return session, nil
}

// buildTeeCore creates a zapcore.Core that fans out to 3 sinks:
// stderr + console.log (simple colored), structured.log (JSON)
// stderr + console.log (simple colored with key=value fields), structured.log (JSON)
func buildTeeCore(level zapcore.Level, noColor bool, consoleFile, structuredFile io.Writer) zapcore.Core {
	consoleCfg := newConsoleEncoderConfig(noColor)
	consoleEncoder := &kvEncoder{
		inner:   zapcore.NewConsoleEncoder(consoleCfg),
		noColor: noColor,
	}

	// JSON encoder config (for structured.log)
	jsonCfg := zap.NewProductionEncoderConfig()
	jsonCfg.TimeKey = "ts"
	jsonCfg.EncodeTime = zapcore.ISO8601TimeEncoder
	jsonEncoder := zapcore.NewJSONEncoder(jsonCfg)

	levelEnabler := zap.LevelEnablerFunc(func(lvl zapcore.Level) bool {
		return lvl >= level
	})

	return zapcore.NewTee(
		zapcore.NewCore(consoleEncoder, zapcore.Lock(os.Stderr), levelEnabler),
		zapcore.NewCore(consoleEncoder.Clone(), zapcore.AddSync(consoleFile), levelEnabler),
		zapcore.NewCore(jsonEncoder, zapcore.AddSync(structuredFile), levelEnabler),
	)
}

// kvEncoder wraps a console encoder and renders structured fields as key=value
// instead of JSON. The inner encoder is stored as a named field (not embedded)
// so that ObjectEncoder.Add* calls from zap's ioCore.With() are intercepted
// here and stored in our fields slice, rather than being baked into the inner
// encoder as JSON.
type kvEncoder struct {
	inner   zapcore.Encoder
	noColor bool
	fields  []zapcore.Field
}

func (e *kvEncoder) Clone() zapcore.Encoder {
	return &kvEncoder{
		inner:   e.inner.Clone(),
		noColor: e.noColor,
		fields:  append([]zapcore.Field{}, e.fields...),
	}
}

func (e *kvEncoder) EncodeEntry(entry zapcore.Entry, fields []zapcore.Field) (*buffer.Buffer, error) {
	all := append(e.fields, fields...)

	// Encode the base line (time + level + message) with no fields
	buf, err := e.inner.EncodeEntry(entry, nil)
	if err != nil {
		return buf, err
	}

	if len(all) > 0 {
		// Remove trailing newline so we can append fields
		data := buf.Bytes()
		if len(data) > 0 && data[len(data)-1] == '\n' {
			buf.TrimNewline()
		}

		for _, f := range all {
			buf.AppendString(" " + f.Key + "=")
			switch f.Type {
			case zapcore.StringType:
				buf.AppendString(f.String)
			case zapcore.Int64Type, zapcore.Int32Type, zapcore.Int16Type, zapcore.Int8Type:
				buf.AppendString(fmt.Sprintf("%d", f.Integer))
			case zapcore.BoolType:
				if f.Integer == 1 {
					buf.AppendString("true")
				} else {
					buf.AppendString("false")
				}
			case zapcore.Float64Type:
				buf.AppendString(fmt.Sprintf("%g", math.Float64frombits(uint64(f.Integer))))
			case zapcore.Float32Type:
				buf.AppendString(fmt.Sprintf("%g", math.Float32frombits(uint32(f.Integer))))
			case zapcore.DurationType:
				buf.AppendString(time.Duration(f.Integer).String())
			case zapcore.ErrorType:
				if f.Interface != nil {
					buf.AppendString(f.Interface.(error).Error())
				}
			default:
				if f.Interface != nil {
					buf.AppendString(fmt.Sprintf("%v", f.Interface))
				}
			}
		}
		buf.AppendString("\n")
	}

	return buf, nil
}

// ObjectEncoder Add* methods = intercept fields from ioCore.With() -> addFields()
// and store them for key=value rendering in EncodeEntry, instead of letting them
// reach the inner console encoder (which would render them as JSON).

func (e *kvEncoder) AddArray(key string, v zapcore.ArrayMarshaler) error {
	e.fields = append(e.fields, zap.Array(key, v))
	return nil
}
func (e *kvEncoder) AddObject(key string, v zapcore.ObjectMarshaler) error {
	e.fields = append(e.fields, zap.Object(key, v))
	return nil
}
func (e *kvEncoder) AddBinary(key string, v []byte) { e.fields = append(e.fields, zap.Binary(key, v)) }
func (e *kvEncoder) AddByteString(key string, v []byte) {
	e.fields = append(e.fields, zap.ByteString(key, v))
}
func (e *kvEncoder) AddBool(key string, v bool) { e.fields = append(e.fields, zap.Bool(key, v)) }
func (e *kvEncoder) AddComplex128(key string, v complex128) {
	e.fields = append(e.fields, zap.Complex128(key, v))
}
func (e *kvEncoder) AddComplex64(key string, v complex64) {
	e.fields = append(e.fields, zap.Complex64(key, v))
}
func (e *kvEncoder) AddDuration(key string, v time.Duration) {
	e.fields = append(e.fields, zap.Duration(key, v))
}
func (e *kvEncoder) AddFloat64(key string, v float64) {
	e.fields = append(e.fields, zap.Float64(key, v))
}
func (e *kvEncoder) AddFloat32(key string, v float32) {
	e.fields = append(e.fields, zap.Float32(key, v))
}
func (e *kvEncoder) AddInt(key string, v int)        { e.fields = append(e.fields, zap.Int(key, v)) }
func (e *kvEncoder) AddInt64(key string, v int64)    { e.fields = append(e.fields, zap.Int64(key, v)) }
func (e *kvEncoder) AddInt32(key string, v int32)    { e.fields = append(e.fields, zap.Int32(key, v)) }
func (e *kvEncoder) AddInt16(key string, v int16)    { e.fields = append(e.fields, zap.Int16(key, v)) }
func (e *kvEncoder) AddInt8(key string, v int8)      { e.fields = append(e.fields, zap.Int8(key, v)) }
func (e *kvEncoder) AddString(key string, v string)  { e.fields = append(e.fields, zap.String(key, v)) }
func (e *kvEncoder) AddTime(key string, v time.Time) { e.fields = append(e.fields, zap.Time(key, v)) }
func (e *kvEncoder) AddUint(key string, v uint)      { e.fields = append(e.fields, zap.Uint(key, v)) }
func (e *kvEncoder) AddUint64(key string, v uint64)  { e.fields = append(e.fields, zap.Uint64(key, v)) }
func (e *kvEncoder) AddUint32(key string, v uint32)  { e.fields = append(e.fields, zap.Uint32(key, v)) }
func (e *kvEncoder) AddUint16(key string, v uint16)  { e.fields = append(e.fields, zap.Uint16(key, v)) }
func (e *kvEncoder) AddUint8(key string, v uint8)    { e.fields = append(e.fields, zap.Uint8(key, v)) }
func (e *kvEncoder) AddUintptr(key string, v uintptr) {
	e.fields = append(e.fields, zap.Uintptr(key, v))
}
func (e *kvEncoder) AddReflected(key string, v interface{}) error {
	e.fields = append(e.fields, zap.Any(key, v))
	return nil
}
func (e *kvEncoder) OpenNamespace(key string) {
	e.fields = append(e.fields, zap.Namespace(key))
}

// newConsoleEncoderConfig returns a simple console encoder config.
// Format: [15:04:05] LEVEL message key=value
func newConsoleEncoderConfig(noColor bool) zapcore.EncoderConfig {
	cfg := zapcore.EncoderConfig{
		TimeKey:          "ts",
		LevelKey:         "level",
		MessageKey:       "msg",
		ConsoleSeparator: " ",
		EncodeDuration:   zapcore.StringDurationEncoder,
	}
	if noColor {
		cfg.EncodeTime = plainTimeEncoder
		cfg.EncodeLevel = paddedLevelEncoder
	} else {
		cfg.EncodeTime = dimTimeEncoder
		cfg.EncodeLevel = colorLevelEncoder
	}
	return cfg
}

// plainTimeEncoder writes time as [15:04:05] (no color).
func plainTimeEncoder(t time.Time, enc zapcore.PrimitiveArrayEncoder) {
	enc.AppendString(t.Format("15:04:05"))
}

// dimTimeEncoder writes time as [15:04:05] in dim.
func dimTimeEncoder(t time.Time, enc zapcore.PrimitiveArrayEncoder) {
	enc.AppendString(colorDim + t.Format("15:04:05") + colorReset)
}

// paddedLevelEncoder writes the level name padded to 5 chars (no color).
func paddedLevelEncoder(l zapcore.Level, enc zapcore.PrimitiveArrayEncoder) {
	enc.AppendString(fmt.Sprintf("%-5s", l.CapitalString()))
}

// colorLevelEncoder writes a colored, padded level name.
func colorLevelEncoder(l zapcore.Level, enc zapcore.PrimitiveArrayEncoder) {
	var color string
	switch l {
	case zapcore.FatalLevel:
		color = colorWhiteOnRd
	case zapcore.ErrorLevel:
		color = colorRed
	case zapcore.WarnLevel:
		color = colorYellow
	case zapcore.DebugLevel:
		color = colorBlue
	default: // Info
		color = colorGreen
	}
	enc.AppendString(color + fmt.Sprintf("%-5s", l.CapitalString()) + colorReset)
}

func parseZapLevel(s string) zapcore.Level {
	switch strings.ToLower(s) {
	case "debug", "trace":
		return zap.DebugLevel
	case "warn", "warning":
		return zap.WarnLevel
	case "error":
		return zap.ErrorLevel
	default:
		return zap.InfoLevel
	}
}

// registerRun appends a pending entry to runs.log
func (s *RunSession) registerRun() {
	runsLogPath := filepath.Join(s.runsLogDir, "runs.log")
	// Ensure parent dir exists
	os.MkdirAll(filepath.Dir(runsLogPath), 0755)

	entry := fmt.Sprintf("%s|%s|%s|pending\n",
		s.StartTime.Format("2006-01-02 15:04:05"),
		s.Config.ClusterName,
		s.RunDir,
	)

	f, err := os.OpenFile(runsLogPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	f.WriteString(entry)
}

// updateLatest writes the current run directory to latest.txt
func (s *RunSession) updateLatest() {
	latestPath := filepath.Join(s.runsLogDir, "latest.txt")
	os.MkdirAll(filepath.Dir(latestPath), 0755)
	os.WriteFile(latestPath, []byte(s.RunDir+"\n"), 0644)
}

// writeHeader writes a session header to all log outputs
func (s *RunSession) writeHeader() {
	s.Logger.Debug("session started",
		zap.String("cluster", s.Config.ClusterName),
		zap.String("config", s.Config.TerraformTFVars),
		zap.String("log_dir", s.RunDir))
}

// Close finalizes the run session: writes SUMMARY.txt, updates runs.log status,
// flushes the logger, and closes all file handles.
func (s *RunSession) Close(exitErr error) {
	duration := time.Since(s.StartTime)

	// Determine status
	status := "success"
	if exitErr != nil {
		status = "failed"
	}

	// Write SUMMARY.txt
	summary := SummaryData{
		StartTime:       s.StartTime,
		Duration:        duration,
		Status:          status,
		ClusterName:     s.Config.ClusterName,
		RunDir:          s.RunDir,
		ExitError:       exitErr,
		ControlPlanes:   s.ControlPlanes,
		Workers:         s.Workers,
		AddedNodes:      s.AddedNodes,
		RemovedNodes:    s.RemovedNodes,
		UpdatedConfigs:  s.UpdatedConfigs,
		BootstrapNeeded: s.BootstrapNeeded,
	}
	WriteSummary(filepath.Join(s.RunDir, "SUMMARY.txt"), &summary)

	// Update runs.log: change last "pending" entry for this run to final status
	s.updateRunsLogStatus(status)

	// Flush zap
	s.Logger.Sync()

	// Close file handles
	for _, c := range s.closers {
		c.Close()
	}
}

// updateRunsLogStatus replaces the status of this run's entry in runs.log
func (s *RunSession) updateRunsLogStatus(status string) {
	runsLogPath := filepath.Join(s.runsLogDir, "runs.log")
	data, err := os.ReadFile(runsLogPath)
	if err != nil {
		return
	}

	lines := strings.Split(string(data), "\n")
	for i, line := range lines {
		if strings.Contains(line, s.RunDir) && strings.HasSuffix(line, "pending") {
			lines[i] = strings.TrimSuffix(line, "pending") + status
			break
		}
	}

	os.WriteFile(runsLogPath, []byte(strings.Join(lines, "\n")), 0644)
}
