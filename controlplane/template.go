package controlplane

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"time"

	"github.com/zybuu-ai/abhed/driver"
)

// Template is a named set of defaults a sandbox is created from: its
// driver, labels and settings. They are copied into the sandbox at create;
// changing or removing the template later leaves existing sandboxes as
// they are.
type Template struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	// Driver, when set, is the driver every sandbox made from it uses; a
	// create naming another is refused.
	Driver string `json:"driver,omitempty"`
	// Labels select the template and are given to each sandbox made from
	// it; a create's own labels win.
	Labels map[string]string `json:"labels,omitempty"`
	// Settings are each sandbox's settings; a create may tighten them but
	// never loosen one.
	Settings        map[string]string `json:"settings,omitempty"`
	Owner           string            `json:"owner"`
	ResourceVersion int64             `json:"resource_version"`
	CreatedAt       time.Time         `json:"created_at"`
}

// Clone copies the template's maps.
func (t Template) Clone() Template {
	t.Labels = maps.Clone(t.Labels)
	t.Settings = maps.Clone(t.Settings)
	return t
}

// TemplateListOptions filters and pages a template list.
type TemplateListOptions struct {
	Selector Selector
	Limit    int
	After    string
}

// TemplateStore keeps templates for one tenant. Memory, SQLite and Postgres
// implement it beside Store, and each must pass storetest.RunTemplates.
type TemplateStore interface {
	// CreateTemplate stores t, which must not exist (ErrExists), with a
	// resource version from the store's counter.
	CreateTemplate(ctx context.Context, t Template) (Template, error)
	// GetTemplate returns the template, or ErrNotFound.
	GetTemplate(ctx context.Context, name string) (Template, error)
	// ListTemplates returns templates in name order and, when more remain,
	// the cursor for the next page.
	ListTemplates(ctx context.Context, o TemplateListOptions) ([]Template, string, error)
	// DeleteTemplate removes the template only when its resource version is
	// expect, else ErrVersionMismatch.
	DeleteTemplate(ctx context.Context, name string, expect int64) error
}

var templateName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)

// ValidTemplateName reports whether n can name a template object.
func ValidTemplateName(n string) bool { return templateName.MatchString(n) }

// PageTemplates applies o's filter and paging to templates in name order.
func PageTemplates(all []Template, o TemplateListOptions) ([]Template, string) {
	size := ListOptions{Limit: o.Limit}.PageSize()
	var out []Template
	for _, t := range all {
		if t.Name <= o.After || !o.Selector.Matches(t.Labels) {
			continue
		}
		if len(out) == size {
			return out, out[len(out)-1].Name
		}
		out = append(out, t.Clone())
	}
	return out, ""
}

// CreateTemplate stores a template in the memory store.
func (m *Memory) CreateTemplate(_ context.Context, t Template) (Template, error) {
	if !ValidTemplateName(t.Name) {
		return Template{}, fmt.Errorf("%w: template name %q", ErrInvalid, t.Name)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.templates == nil {
		m.templates = map[string]Template{}
	}
	if _, ok := m.templates[t.Name]; ok {
		return Template{}, fmt.Errorf("%w: template %s", ErrExists, t.Name)
	}
	m.rv++
	t = t.Clone()
	t.ResourceVersion = m.rv
	m.templates[t.Name] = t
	return t.Clone(), nil
}

func (m *Memory) GetTemplate(_ context.Context, name string) (Template, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.templates[name]
	if !ok {
		return Template{}, fmt.Errorf("%w: template %s", ErrNotFound, name)
	}
	return t.Clone(), nil
}

func (m *Memory) ListTemplates(_ context.Context, o TemplateListOptions) ([]Template, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	all := slices.Collect(maps.Values(m.templates))
	slices.SortFunc(all, func(a, b Template) int {
		switch {
		case a.Name < b.Name:
			return -1
		case a.Name > b.Name:
			return 1
		}
		return 0
	})
	out, next := PageTemplates(all, o)
	return out, next, nil
}

func (m *Memory) DeleteTemplate(_ context.Context, name string, expect int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.templates[name]
	if !ok {
		return fmt.Errorf("%w: template %s", ErrNotFound, name)
	}
	if t.ResourceVersion != expect {
		return fmt.Errorf("%w: template %s is at %d, not %d", ErrVersionMismatch, name, t.ResourceVersion, expect)
	}
	delete(m.templates, name)
	return nil
}

// templates is the service's template store, or ErrUnsupported when its
// store keeps none.
func (s *Service) templates() (TemplateStore, error) {
	ts, ok := s.cfg.Store.(TemplateStore)
	if !ok {
		return nil, fmt.Errorf("%w: this server's store keeps no templates", driver.ErrUnsupported)
	}
	return ts, nil
}

// mayWriteTemplates: templates shape every caller's sandboxes, so only an
// administrator of the whole tenant changes them.
func (c Caller) mayWriteTemplates() error {
	if !c.Admin || len(c.Scope) > 0 {
		return fmt.Errorf("%w: only an administrator of this server changes templates", ErrForbidden)
	}
	return nil
}

// checkTemplate validates a template's fields as a create would use them.
func (s *Service) checkTemplate(t Template) error {
	if !ValidTemplateName(t.Name) {
		return fmt.Errorf("%w: a template name is 1 to 63 letters, digits, '.', '_' or '-', starting with a letter or digit", ErrInvalid)
	}
	if len(t.Description) > 1024 {
		return fmt.Errorf("%w: a description is at most 1024 bytes", ErrInvalid)
	}
	if t.Driver != "" {
		if _, err := s.cfg.Drivers.Get(t.Driver); err != nil {
			return fmt.Errorf("%w: no driver named %q", ErrInvalid, t.Driver)
		}
	}
	if err := ValidateLabels(t.Labels, false); err != nil {
		return err
	}
	return s.cfg.Settings.Check(t.Settings)
}

// CreateTemplate stores a new template, recorded as a mutation.
func (s *Service) CreateTemplate(ctx context.Context, c Caller, t Template) (Template, error) {
	ts, err := s.templates()
	if err != nil {
		return Template{}, err
	}
	if err := c.mayWriteTemplates(); err != nil {
		return Template{}, err
	}
	if err := s.checkTemplate(t); err != nil {
		return Template{}, err
	}
	t.Owner, t.CreatedAt, t.ResourceVersion = c.Principal, s.now(), 0
	ev := AuditEvent{Principal: c.Principal, Action: "template.create",
		Detail: map[string]string{"template": t.Name, "driver": t.Driver}}
	if err := s.record(ctx, requested(ev)); err != nil {
		return Template{}, err
	}
	out, err := ts.CreateTemplate(ctx, t)
	if err != nil {
		err = storeErr(err)
	}
	s.outcome(ctx, ev, err)
	return out, err
}

// GetTemplate returns a template; every caller of the tenant may read one.
func (s *Service) GetTemplate(ctx context.Context, _ Caller, name string) (Template, error) {
	ts, err := s.templates()
	if err != nil {
		return Template{}, err
	}
	t, err := ts.GetTemplate(ctx, name)
	if err != nil {
		return Template{}, storeErr(err)
	}
	return t, nil
}

// ListTemplates lists templates matching o.
func (s *Service) ListTemplates(ctx context.Context, _ Caller, o TemplateListOptions) ([]Template, string, error) {
	ts, err := s.templates()
	if err != nil {
		return nil, "", err
	}
	out, next, err := ts.ListTemplates(ctx, o)
	if err != nil {
		return nil, "", storeErr(err)
	}
	return out, next, nil
}

// DeleteTemplate removes a template; sandboxes made from it keep what they
// were given.
func (s *Service) DeleteTemplate(ctx context.Context, c Caller, name, ifMatch string) error {
	ts, err := s.templates()
	if err != nil {
		return err
	}
	if err := c.mayWriteTemplates(); err != nil {
		return err
	}
	cur, err := ts.GetTemplate(ctx, name)
	if err != nil {
		return storeErr(err)
	}
	if err := CheckIfMatch(ifMatch, cur.ResourceVersion); err != nil {
		return err
	}
	ev := AuditEvent{Principal: c.Principal, Action: "template.delete", Detail: map[string]string{"template": name}}
	if err := s.record(ctx, requested(ev)); err != nil {
		return err
	}
	err = ts.DeleteTemplate(ctx, name, cur.ResourceVersion)
	if errors.Is(err, ErrVersionMismatch) {
		err = fmt.Errorf("%w: the template changed while it was being removed", ErrPreconditionFailed)
	} else if err != nil {
		err = storeErr(err)
	}
	s.outcome(ctx, ev, err)
	return err
}

// applyTemplate fills a create from the template it names, when the store
// keeps one by that name. A name with no template object stays a name, as
// before templates were kept. The template's driver cannot be swapped, and
// its settings can only be tightened.
func (s *Service) applyTemplate(ctx context.Context, req CreateRequest) (CreateRequest, error) {
	ts, ok := s.cfg.Store.(TemplateStore)
	if !ok || !ValidTemplateName(req.Template) {
		return req, nil
	}
	t, err := ts.GetTemplate(ctx, req.Template)
	if errors.Is(err, ErrNotFound) {
		return req, nil
	}
	if err != nil {
		return req, storeErr(err)
	}
	if t.Driver != "" {
		if req.Driver != "" && req.Driver != t.Driver {
			return req, fmt.Errorf("%w: template %s runs on driver %q, not %q", ErrInvalid, t.Name, t.Driver, req.Driver)
		}
		req.Driver = t.Driver
	}
	labels := maps.Clone(t.Labels)
	if labels == nil && len(req.Labels) > 0 {
		labels = map[string]string{}
	}
	maps.Copy(labels, req.Labels)
	req.Labels = labels
	settings := maps.Clone(t.Settings)
	for k, v := range req.Settings {
		if was, set := t.Settings[k]; set {
			if err := s.cfg.Settings.NoLooser(map[string]string{k: was}, map[string]string{k: v}); err != nil {
				return req, fmt.Errorf("%w (template %s sets it)", err, t.Name)
			}
		}
		if settings == nil {
			settings = map[string]string{}
		}
		settings[k] = v
	}
	req.Settings = settings
	return req, nil
}

// EventsOptions pages a sandbox's record.
type EventsOptions struct {
	// After is the last sequence number already read.
	After int64
	Limit int
}

// Events lists a sandbox's recorded events, oldest first, when the caller
// may see the sandbox.
func (s *Service) Events(ctx context.Context, c Caller, id string, o EventsOptions) ([]AuditEvent, int64, error) {
	if _, err := s.Get(ctx, c, id); err != nil {
		return nil, 0, err
	}
	rd, ok := s.cfg.Audit.(AuditReader)
	if !ok {
		return nil, 0, fmt.Errorf("%w: this server's record cannot be read back", driver.ErrUnsupported)
	}
	all, err := rd.AuditFor(ctx, id)
	if err != nil {
		return nil, 0, storeErr(err)
	}
	size := ListOptions{Limit: o.Limit}.PageSize()
	var out []AuditEvent
	for _, ev := range all {
		if ev.Seq <= o.After {
			continue
		}
		if len(out) == size {
			return out, out[len(out)-1].Seq, nil
		}
		out = append(out, ev)
	}
	return out, 0, nil
}
