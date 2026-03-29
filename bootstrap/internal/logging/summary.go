package logging

import (
	"fmt"
	"os"
	"time"
)

// SummaryData holds all information needed to generate a SUMMARY.txt.
type SummaryData struct {
	StartTime   time.Time
	Duration    time.Duration
	Status      string
	ClusterName string
	RunDir      string
	ExitError   error

	// Optional operational counts (set by caller before Close)
	ControlPlanes   int
	Workers         int
	AddedNodes      int
	RemovedNodes    int
	UpdatedConfigs  int
	BootstrapNeeded bool
}

// WriteSummary generates a SUMMARY.txt at the given path.
func WriteSummary(path string, data *SummaryData) error {
	var errMsg string
	if data.ExitError != nil {
		errMsg = data.ExitError.Error()
	} else {
		errMsg = "none"
	}

	content := fmt.Sprintf(`━━━ TALOS BOOTSTRAP RUN SUMMARY ━━━
  Status:       %s
  Cluster:      %s
  Start:        %s
  Duration:     %s
  Run Dir:      %s
  Error:        %s
  ─── Cluster Nodes ───
  Control Planes: %d
  Workers:        %d
  ─── Operations ───
  Added:          %d
  Removed:        %d
  Updated:        %d
  Bootstrap:      %v
━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
`,
		data.Status,
		data.ClusterName,
		data.StartTime.Format("2006-01-02 15:04:05"),
		data.Duration.Round(time.Second),
		data.RunDir,
		errMsg,
		data.ControlPlanes,
		data.Workers,
		data.AddedNodes,
		data.RemovedNodes,
		data.UpdatedConfigs,
		data.BootstrapNeeded,
	)

	return os.WriteFile(path, []byte(content), 0644)
}
