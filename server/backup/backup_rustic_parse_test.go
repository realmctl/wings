package backup

import "testing"

// Real output shape from `rustic backup --tag X --as-path / --json` (rustic 0.11.3).
const rusticBackupJSON = `{
  "time": "2026-07-03T17:39:52.479048267+00:00",
  "tree": "19c0c20752",
  "paths": ["/"],
  "tags": ["UUID123"],
  "summary": {"total_files_processed": 2, "total_bytes_processed": 19, "data_added": 1050},
  "id": "cbcf2171ac67841b5fe1dd4a1b1cb63b1b3ed3424b6c49040daffe98c1103c97"
}`

// Real output shape from `rustic snapshots --filter-tags-exact X --json` (grouped).
const rusticSnapshotsJSON = `[
  {
    "group_key": {"hostname": "h", "label": "", "paths": ["/"]},
    "snapshots": [
      {"time": "2026-07-03T17:39:52Z", "paths": ["/"], "tags": ["UUID123"],
       "summary": {"total_bytes_processed": 19},
       "id": "cbcf2171ac67841b5fe1dd4a1b1cb63b1b3ed3424b6c49040daffe98c1103c97"}
    ]
  }
]`

func TestParseSnapshotBackupOutput(t *testing.T) {
	snap, err := parseSnapshot([]byte(rusticBackupJSON))
	if err != nil {
		t.Fatalf("parseSnapshot: %v", err)
	}
	if snap.Id != "cbcf2171ac67841b5fe1dd4a1b1cb63b1b3ed3424b6c49040daffe98c1103c97" {
		t.Fatalf("bad id: %q", snap.Id)
	}
	if snap.Summary == nil || snap.Summary.TotalBytesProcessed != 19 {
		t.Fatalf("bad summary: %+v", snap.Summary)
	}
}

func TestParseSnapshotsGrouped(t *testing.T) {
	snaps, err := parseSnapshots([]byte(rusticSnapshotsJSON))
	if err != nil {
		t.Fatalf("parseSnapshots: %v", err)
	}
	if len(snaps) != 1 {
		t.Fatalf("expected 1 snapshot, got %d", len(snaps))
	}
	if snaps[0].Id == "" || snaps[0].Summary == nil || snaps[0].Summary.TotalBytesProcessed != 19 {
		t.Fatalf("bad parsed snapshot: %+v", snaps[0])
	}
}
