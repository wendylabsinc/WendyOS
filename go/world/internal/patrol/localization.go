package patrol

// LocalizationHealth carries observation diagnostics to the viewer. It is not
// an authorization input: recovered observations never resume a held mission.
// Nullable ages preserve missing observations rather than displaying zero age.
type LocalizationHealth struct {
	Schema        string    `json:"schema"`
	Ready         bool      `json:"ready"`
	Reasons       []string  `json:"reasons"`
	SensorAge     *float64  `json:"sensor_age_s"`
	PoseAge       *float64  `json:"pose_age_s"`
	CostmapAge    *float64  `json:"costmap_age_s"`
	Variance      []float64 `json:"variance"`
	VarianceLimit float64   `json:"variance_limit"`
	Passive       struct {
		Attempts       uint64   `json:"attempts"`
		Pending        bool     `json:"pending"`
		LastScanStamp  *float64 `json:"last_scan_stamp"`
		LastAttemptAge *float64 `json:"last_attempt_age_s"`
	} `json:"passive"`
	MotionResumed bool `json:"motion_resumed"`
}
