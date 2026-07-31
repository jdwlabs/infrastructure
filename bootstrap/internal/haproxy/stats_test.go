package haproxy

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A trimmed but structurally faithful sample of what "show stat" returns: the
// commented header, the FRONTEND/BACKEND aggregate rows, and per-server rows.
const sampleStatsCSV = `# pxname,svname,qcur,scur,status,weight,chkfail,downtime,check_status,addr,rate,
stats,FRONTEND,0,0,OPEN,,,,,,0,
k8s-apiserver,FRONTEND,0,1,OPEN,,,,,,3,
k8s-controlplane,talos-cp-201,0,1,UP,1,0,0,L4OK,192.168.1.21:6443,3,
k8s-controlplane,talos-cp-202,0,0,DOWN,1,4,182,L4TOUT,192.168.1.22:6443,0,
k8s-controlplane,BACKEND,0,1,UP,2,,182,,,3,
talos-controlplane,talos-cp-201,0,0,UP,1,0,0,L4OK,192.168.1.21:50000,0,
`

func TestParseStatsSkipsAggregateRows(t *testing.T) {
	stats, err := ParseStats(sampleStatsCSV)
	require.NoError(t, err)

	require.Len(t, stats, 3, "FRONTEND and BACKEND rows duplicate the per-server data and must not be counted")
	for _, s := range stats {
		assert.NotContains(t, []string{"FRONTEND", "BACKEND"}, s.Name())
	}
}

func TestParseStatsReadsColumnsByHeaderName(t *testing.T) {
	stats, err := ParseStats(sampleStatsCSV)
	require.NoError(t, err)

	assert.Equal(t, "talos-cp-201", stats[0].Name())
	assert.Equal(t, "192.168.1.21:6443", stats[0].Field("addr"))
	assert.Equal(t, "UP", stats[0].Status())
	assert.Equal(t, "L4OK", stats[0].Field("check"))
	assert.Equal(t, "L4TOUT", stats[1].Field("check"))
}

// The column an index points at shifts when HAProxy inserts a new one, so
// positions must come from the header of the response being parsed.
func TestParseStatsToleratesReorderedColumns(t *testing.T) {
	reordered := `# svname,status,pxname,addr,check_status,
talos-cp-201,UP,k8s-controlplane,192.168.1.21:6443,L4OK,
`
	stats, err := ParseStats(reordered)
	require.NoError(t, err)
	require.Len(t, stats, 1)

	assert.Equal(t, "talos-cp-201", stats[0].Name())
	assert.Equal(t, "UP", stats[0].Status())
	assert.Equal(t, "k8s-controlplane", stats[0].Field("proxy"))
}

func TestParseStatsRejectsOutputWithoutHeader(t *testing.T) {
	_, err := ParseStats("Permission denied\n")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no CSV header")
}

func TestParseStatsEmptyOutputIsAnError(t *testing.T) {
	_, err := ParseStats("")
	require.Error(t, err)
}

// A server transitioning back into rotation reports "UP 2/3". Counting it as
// down would report a healthy load balancer as degraded during every reload.
func TestIsUpAcceptsTransitionalState(t *testing.T) {
	stats, err := ParseStats(`# svname,status,
talos-cp-201,UP 2/3,
talos-cp-202,DOWN 1/3,
talos-cp-203,MAINT,
talos-cp-204,no check,
`)
	require.NoError(t, err)
	assert.Equal(t, 1, UpCount(stats))
}

func TestFieldReportsMissingColumnsAsDash(t *testing.T) {
	stats, err := ParseStats("# svname,status,\ntalos-cp-201,UP,\n")
	require.NoError(t, err)

	assert.Equal(t, "-", stats[0].Field("addr"), "an absent column is better shown than dropped")
	assert.Equal(t, "-", stats[0].Field("check"))
}

func TestResolveStatFieldsDefaultsToTheMinimalSchema(t *testing.T) {
	stats, err := ParseStats(sampleStatsCSV)
	require.NoError(t, err)

	fields, err := ResolveStatFields(nil, stats)
	require.NoError(t, err)
	assert.Equal(t, []string{"name", "addr", "status", "check"}, fields)
}

func TestResolveStatFieldsAcceptsAliasesAndRawColumnNames(t *testing.T) {
	stats, err := ParseStats(sampleStatsCSV)
	require.NoError(t, err)

	fields, err := ResolveStatFields([]string{"name", "downtime", "chkfail"}, stats)
	require.NoError(t, err)
	assert.Equal(t, []string{"name", "downtime", "chkfail"}, fields)
}

// A silently dropped field returns a table the caller believes is what they
// asked for, which is worse than an error.
func TestResolveStatFieldsRejectsUnknownFieldByName(t *testing.T) {
	stats, err := ParseStats(sampleStatsCSV)
	require.NoError(t, err)

	_, err = ResolveStatFields([]string{"name", "helth"}, stats)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `"helth"`)
	assert.Contains(t, err.Error(), "valid names:", "the error must carry the correction inline")
	assert.Contains(t, err.Error(), "status")
}

func TestAllStatFieldsCoversEveryColumnTheHostEmitted(t *testing.T) {
	stats, err := ParseStats(sampleStatsCSV)
	require.NoError(t, err)

	all := AllStatFields(stats)
	assert.Contains(t, all, "chkfail")
	assert.Contains(t, all, "svname")
	assert.Greater(t, len(all), len(DefaultStatFields))
}

func TestParseStatsHandlesCRLF(t *testing.T) {
	stats, err := ParseStats(strings.ReplaceAll(sampleStatsCSV, "\n", "\r\n"))
	require.NoError(t, err)
	assert.Len(t, stats, 3)
}
