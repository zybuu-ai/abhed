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
}

// Report checks t and plans its next step. The registry and download
// hosts are asked for sizes only when that step may be offered, so with
// assist off or local nothing leaves the machine.
func (a Assist) Report(ctx context.Context, t Target) Report {
	st := a.Env.Check(ctx, t)
	r := Report{Status: st, Assist: a.Mode, StartAlways: a.Env.StartAlways(), Catalog: Catalog}
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
	case OpInstall:
		plan := a.Env.PlanInstall()
		a.Env.InstallSize(ctx, &plan)
		r.Install = &plan
		if plan.Kind == InstallManual {
			r.CanAct = false
		}
	case OpPull:
		fit, p := a.Env.CheckFit(ctx, t, st)
		if p != nil {
			r.CanAct, r.Refusal = false, p
			return r
		}
		r.Pull = &PullPlan{Command: t.PullCommand(), Source: a.Env.Registry, Fit: fit}
	}
	return r
}
