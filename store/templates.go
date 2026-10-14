package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/zybuu-ai/abhed/controlplane"
)

var _ controlplane.TemplateStore = (*Sandboxes)(nil)

func scanTemplate(row pgx.Row) (controlplane.Template, error) {
	var raw []byte
	var rv int64
	if err := row.Scan(&raw, &rv); err != nil {
		return controlplane.Template{}, err
	}
	var t controlplane.Template
	if err := json.Unmarshal(raw, &t); err != nil {
		return controlplane.Template{}, fmt.Errorf("decode template: %w", err)
	}
	t.ResourceVersion = rv
	return t, nil
}

func (q *Sandboxes) CreateTemplate(ctx context.Context, t controlplane.Template) (controlplane.Template, error) {
	if !controlplane.ValidTemplateName(t.Name) {
		return controlplane.Template{}, fmt.Errorf("%w: template name %q", controlplane.ErrInvalid, t.Name)
	}
	t.ResourceVersion = 0
	raw, err := json.Marshal(t)
	if err != nil {
		return controlplane.Template{}, err
	}
	var rv int64
	err = q.pool.QueryRow(ctx, `
		INSERT INTO sandbox_templates (tenant_id, name, resource_version, object)
		VALUES ($1, $2, nextval('sandbox_resource_version'), $3)
		RETURNING resource_version`, q.tenant, t.Name, raw).Scan(&rv)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return controlplane.Template{}, fmt.Errorf("%w: template %s", controlplane.ErrExists, t.Name)
		}
		return controlplane.Template{}, fmt.Errorf("create template: %w", err)
	}
	t = t.Clone()
	t.ResourceVersion = rv
	return t, nil
}

func (q *Sandboxes) GetTemplate(ctx context.Context, name string) (controlplane.Template, error) {
	t, err := scanTemplate(q.pool.QueryRow(ctx,
		`SELECT object, resource_version FROM sandbox_templates WHERE tenant_id = $1 AND name = $2`, q.tenant, name))
	if errors.Is(err, pgx.ErrNoRows) {
		return controlplane.Template{}, fmt.Errorf("%w: template %s", controlplane.ErrNotFound, name)
	}
	return t, err
}

// ListTemplates reads the tenant's templates and pages them as the memory
// store does; a server keeps few.
func (q *Sandboxes) ListTemplates(ctx context.Context, o controlplane.TemplateListOptions) ([]controlplane.Template, string, error) {
	rows, err := q.pool.Query(ctx, `
		SELECT object, resource_version FROM sandbox_templates
		WHERE tenant_id = $1 AND name > $2 ORDER BY name`, q.tenant, o.After)
	if err != nil {
		return nil, "", fmt.Errorf("list templates: %w", err)
	}
	defer rows.Close()
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

func (q *Sandboxes) DeleteTemplate(ctx context.Context, name string, expect int64) error {
	tag, err := q.pool.Exec(ctx, `DELETE FROM sandbox_templates WHERE tenant_id = $1 AND name = $2 AND resource_version = $3`,
		q.tenant, name, expect)
	if err != nil {
		return fmt.Errorf("delete template: %w", err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	cur, err := q.GetTemplate(ctx, name)
	if err != nil {
		return err
	}
	return fmt.Errorf("%w: template %s is at %d, not %d", controlplane.ErrVersionMismatch, name, cur.ResourceVersion, expect)
}
