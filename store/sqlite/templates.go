package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/zybuu-ai/abhed/controlplane"
)

var _ controlplane.TemplateStore = (*Store)(nil)

func scanTemplate(row scanner) (controlplane.Template, error) {
	var raw string
	var rv int64
	if err := row.Scan(&raw, &rv); err != nil {
		return controlplane.Template{}, err
	}
	var t controlplane.Template
	if err := json.Unmarshal([]byte(raw), &t); err != nil {
		return controlplane.Template{}, fmt.Errorf("decode template: %w", err)
	}
	t.ResourceVersion = rv
	return t, nil
}

func (s *Store) CreateTemplate(ctx context.Context, t controlplane.Template) (controlplane.Template, error) {
	if !controlplane.ValidTemplateName(t.Name) {
		return controlplane.Template{}, fmt.Errorf("%w: template name %q", controlplane.ErrInvalid, t.Name)
	}
	t.ResourceVersion = 0
	raw, err := json.Marshal(t)
	if err != nil {
		return controlplane.Template{}, err
	}
	var rv int64
	err = s.write(ctx, "create template", func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM sandbox_templates WHERE name = ?`, t.Name).Scan(&n); err != nil {
			return fmt.Errorf("create template: %w", err)
		}
		if n > 0 {
			return fmt.Errorf("%w: template %s", controlplane.ErrExists, t.Name)
		}
		if rv, err = nextVersion(ctx, tx); err != nil {
			return fmt.Errorf("create template: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO sandbox_templates (name, resource_version, object) VALUES (?, ?, ?)`,
			t.Name, rv, string(raw)); err != nil {
			return fmt.Errorf("create template: %w", err)
		}
		return nil
	})
	if err != nil {
		return controlplane.Template{}, err
	}
	t = t.Clone()
	t.ResourceVersion = rv
	return t, nil
}

func (s *Store) GetTemplate(ctx context.Context, name string) (controlplane.Template, error) {
	t, err := scanTemplate(s.r.QueryRowContext(ctx, `SELECT object, resource_version FROM sandbox_templates WHERE name = ?`, name))
	if errors.Is(err, sql.ErrNoRows) {
		return controlplane.Template{}, fmt.Errorf("%w: template %s", controlplane.ErrNotFound, name)
	}
	return t, err
}

// ListTemplates reads every template and pages them as the memory store
// does; a server keeps few.
func (s *Store) ListTemplates(ctx context.Context, o controlplane.TemplateListOptions) ([]controlplane.Template, string, error) {
	rows, err := s.r.QueryContext(ctx, `SELECT object, resource_version FROM sandbox_templates WHERE name > ? ORDER BY name`, o.After)
	if err != nil {
		return nil, "", fmt.Errorf("list templates: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var all []controlplane.Template
	for rows.Next() {
		t, err := scanTemplate(rows)
		if err != nil {
			return nil, "", err
		}
		all = append(all, t)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("list templates: %w", err)
	}
	out, next := controlplane.PageTemplates(all, o)
	return out, next, nil
}

func (s *Store) DeleteTemplate(ctx context.Context, name string, expect int64) error {
	return s.write(ctx, "delete template", func(tx *sql.Tx) error {
		var rv int64
		err := tx.QueryRowContext(ctx, `SELECT resource_version FROM sandbox_templates WHERE name = ?`, name).Scan(&rv)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: template %s", controlplane.ErrNotFound, name)
		}
		if err != nil {
			return fmt.Errorf("delete template: %w", err)
		}
		if rv != expect {
			return fmt.Errorf("%w: template %s is at %d, not %d", controlplane.ErrVersionMismatch, name, rv, expect)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM sandbox_templates WHERE name = ?`, name); err != nil {
			return fmt.Errorf("delete template: %w", err)
		}
		return nil
	})
}
