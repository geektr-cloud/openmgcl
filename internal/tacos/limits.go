package tacos

// Request body limits. Raw state uploads are bounded to roughly 100 MiB; the
// create-state-version body must also hold base64-encoded state and JSON state,
// so it gets headroom for up to three inline base64 fields plus JSON overhead.
const (
	maxStateUploadBytes           int64 = 100 << 20
	maxStateVersionCreateBodySize int64 = 3*((maxStateUploadBytes+2)/3*4) + (1 << 20)
	maxWorkspaceBodySize          int64 = 1 << 20
	maxLockBodySize               int64 = 1 << 20
	maxPageSize                         = 100
)
