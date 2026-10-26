package ollama

import "context"

// PullPlan is a download as the person is shown it before agreeing.
type PullPlan struct {
	Command string `json:"command"`
	Source  string `json:"source"`
	Fit
}

// Report is everything a surface shows for a chosen Ollama model: what was
// found, the next step and exactly what it would run, or why Abhed will
// not run it, with the manual steps either way.
type Report struct {
	Status
	Assist string `json:"assist"`
	// CanAct is set when the next step may be offered here.
	CanAct      bool `json:"can_act"`
	StartAlways bool `json:"start_always"`
	// The plan of the next step, when it may be offered.
	Start   *StartPlan   `json:"start,omitempty"`
	Install *InstallPlan `json:"install,omitempty"`
	Pull    *PullPlan    `json:"pull,omitempty"`
	// Refusal says why the next step is not offered; Status.Problem still
	// says what is wrong.
	Refusal *Problem  `json:"refusal,omitempty"`
	Catalog []Curated `json:"catalog"`
	// Detail is set when the registry or download host was asked for the
	// size; otherwise sizes are Abhed's own list's or unknown.
	Detail bool `json:"detail"`
}

// Report checks t and plans its next step. Only this machine is asked
// unless detail is set: the local Ollama, the disk and the memory, with a
// curated model's size from Abhed's own list. detail, for when the person
// opens the install or pull step, also asks the registry or the download
// host for the size as it is now; never when that step may not be offered,
// so with assist off or local nothing leaves the machine.
func (a Assist) Report(ctx context.Context, t Target, detail bool) Report {
	st := a.Env.Check(ctx, t)
	r := Report{Status: st, Assist: a.Mode, Catalog: Catalog, Detail: detail}
	if st.Next == "" {
		return r
	}
	op := map[string]string{"start": OpStart, "install": OpInstall, "pull": OpPull}[st.Next]
	if p := a.May(op, t, st); p != nil {
		r.Refusal = p
		return r
	}
	r.CanAct = true
	switch op {
	case OpStart:
		plan, p := a.Env.PlanStart(t, st)
		if p != nil {
			r.CanAct, r.Refusal = false, p
			return r
		}
		r.Start = &plan
		r.StartAlways = a.Env.StartAlways(plan.Program)
	case OpInstall:
		plan := a.Env.PlanInstall()
		if detail {
			a.Env.InstallSize(ctx, &plan)
		}
		r.Install = &plan
		if plan.Kind == InstallManual {
			r.CanAct = false
		}
	case OpPull:
		fit := a.Env.LocalFit(t)
		var p *Problem
		if detail {
			fit, p = a.Env.CheckFit(ctx, t, st)
		}
		if p != nil {
			r.CanAct, r.Refusal = false, p
			return r
		}
		r.Pull = &PullPlan{Command: t.PullCommand(), Source: a.Env.Registry, Fit: fit}
	}
	return r
}
