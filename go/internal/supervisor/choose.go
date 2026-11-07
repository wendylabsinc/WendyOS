package supervisor

import (
	"fmt"
	"slices"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/version"
)

const recheckReason = "re-checking the agent on a new OS build"

const (
	TrialFailureLimit    = 3
	ApprovedFailureLimit = 5
	// MaxTrialBoots bounds a trial that takes the whole device down with it.
	MaxTrialBoots = 2
	// LongRun is how long an agent must run before a crash stops counting
	// against it, so a rare crash days apart never demotes a good agent.
	LongRun = 10 * time.Minute
)

type Role string

const (
	RoleImage    Role = "image"
	RoleApproved Role = "approved"
	RoleTrial    Role = "trial"
)

type Inputs struct {
	// Emergency forces the image agent (a flag on /config or the kernel cmdline).
	Emergency bool
	// ForceImage, when set, is why this start must use the image agent.
	ForceImage   string
	DataMounted  bool
	StateErr     error
	State        State
	Build        string
	BootID       string
	ImageVersion string
	// Uptime is the boot clock now; TrialDeadline is how long a trial may take.
	Uptime        time.Duration
	TrialDeadline time.Duration
	// OSUnconfirmed: the OS gate could not confirm this boot's update, so a
	// revert may still bring back the build the approved agent ran on.
	OSUnconfirmed bool
}

type Decision struct {
	Role         Role
	SHA256       string
	Version      string
	Reason       string
	Revalidation bool
	Automatic    bool
	// Grace: a trial that passed its checks and is reported approved.
	Grace bool
	// TrialRejected is why Choose rejected the trial it was given, if it did.
	TrialRejected string
}

// Choose picks the agent to run and returns the state to persist before it
// starts; if that write fails, the caller must run the image agent instead.
func Choose(in Inputs) (Decision, State) {
	st := in.State
	switch {
	case in.Emergency:
		return image("emergency flag set"), st
	case in.ForceImage != "":
		return image(in.ForceImage), st
	case !in.DataMounted:
		return image("/data is not mounted"), st
	case in.StateErr != nil:
		return image(fmt.Sprintf("agent state unreadable: %v", in.StateErr)), st
	}

	if t := st.Trial; t != nil && t.Confirmed && t.Build != in.Build {
		// It passed on another OS build; here it is the approved agent and is
		// judged as one, so a newer image agent still wins.
		st.Promote(t.Build)
	}
	if st.Trial != nil {
		t := *st.Trial
		if !slices.Contains(t.Boots, in.BootID) {
			t.Boots = append(slices.Clone(t.Boots), in.BootID)
			t.StartedAt = in.Uptime
		}
		reason := ""
		switch n := st.FailureCount(t.SHA256, in.BootID); {
		case len(t.Boots) > MaxTrialBoots:
			reason = fmt.Sprintf("the device restarted %d times during its trial", len(t.Boots)-1)
		case n >= TrialFailureLimit:
			reason = fmt.Sprintf("failed %d times during its trial", n)
		case !t.Confirmed && in.TrialDeadline > 0 && in.Uptime-t.StartedAt >= in.TrialDeadline:
			// A confirmed trial past its deadline is approved by the run, once it answers again.
			reason = fmt.Sprintf("did not pass its checks within %s", in.TrialDeadline)
		}
		if reason == "" {
			st.Trial = &t
			why := "testing a new agent"
			if t.Revalidation {
				why = recheckReason
			}
			return Decision{Role: RoleTrial, SHA256: t.SHA256, Version: t.Version, Reason: why,
				Revalidation: t.Revalidation, Automatic: t.Automatic, Grace: t.Confirmed}, st
		}
		st.Reject(t.SHA256, t.Version, in.Build, reason)
		st.Trial = nil
		d, st := chooseApproved(in, st)
		d.TrialRejected = reason
		return d, st
	}
	return chooseApproved(in, st)
}

func chooseApproved(in Inputs, st State) (Decision, State) {
	a := st.Approved
	if a == nil {
		return image("no agent installed on /data"), st
	}
	// A missed remote confirm, as of a re-sent copy, says nothing against it.
	if st.hardRejected(a.SHA256, in.Build) {
		reason := fmt.Sprintf("agent %s failed its checks on this OS build", a.Version)
		if why := st.rejectionReason(a.SHA256, in.Build); why != "" {
			reason += ": " + why
		}
		return image(reason), st
	}
	// An unknown build never counts as validated, so it is re-checked every start.
	if in.Build != "" && slices.Contains(a.Builds, in.Build) {
		if n := st.FailureCount(a.SHA256, in.BootID); n >= ApprovedFailureLimit {
			return image(fmt.Sprintf("agent %s failed %d times this boot", a.Version, n)), st
		}
		return Decision{Role: RoleApproved, SHA256: a.SHA256, Version: a.Version, Reason: "approved agent"}, st
	}
	if version.CompareVersions(in.ImageVersion, a.Version) >= 0 {
		if !in.OSUnconfirmed {
			st.Approved = nil
		}
		return image(fmt.Sprintf("image agent %s is the same or newer than %s", in.ImageVersion, a.Version)), st
	}
	// It already proved remote reachability when it was approved.
	st.Trial = &Trial{Agent: *a, Revalidation: true, Boots: []string{in.BootID}, StartedAt: in.Uptime}
	return Decision{Role: RoleTrial, SHA256: a.SHA256, Version: a.Version, Reason: recheckReason, Revalidation: true}, st
}

func image(reason string) Decision {
	return Decision{Role: RoleImage, Reason: reason}
}

type Exit struct {
	Code     int
	Signaled bool
	// Stopped means the supervisor ended the run itself.
	Stopped bool
	Ran     time.Duration
}

// CountsAsFailure ignores clean exits, which the agent uses to restart itself
// (certificate renewal, unenrollment, a new upload in the inbox).
func CountsAsFailure(e Exit) bool {
	if e.Stopped || e.Ran >= LongRun {
		return false
	}
	return e.Signaled || e.Code != 0
}
