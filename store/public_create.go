package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Postgres before 15 lets every role create objects in schema public. A role
// that can put a function or operator there may shadow the one a trigger
// means (CVE-2018-1058), or make a table a later migration expects, so the
// runtime role must not be able to, by any route.

type queryRower interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

// publicCreate says how a role can create in schema public.
type publicCreate struct {
	can      bool     // through any role it is a member of, INHERIT or not
	public   bool     // PUBLIC holds CREATE, as before Postgres 15
	direct   bool     // granted to the role itself
	groups   []string // granted to roles it belongs to
	owner    bool     // it is, or belongs to, the schema's owner
	nspOwner string   // the schema's owner (pg_database_owner on 15 and later)
}

// publicCreateOf looks at role's routes to CREATE on schema public. Every
// role it is a member of counts, with INHERIT or without, since SET ROLE
// reaches them all.
func publicCreateOf(ctx context.Context, q queryRower, role string) (publicCreate, error) {
	var pc publicCreate
	err := q.QueryRow(ctx, `
		WITH me AS (SELECT oid FROM pg_catalog.pg_roles WHERE rolname = $1),
		     n AS (SELECT nspowner, nspacl FROM pg_catalog.pg_namespace WHERE nspname = 'public'),
		     acl AS (SELECT a.grantee FROM n, aclexplode(n.nspacl) a WHERE a.privilege_type = 'CREATE')
		SELECT
		  COALESCE((SELECT bool_or(pg_catalog.has_schema_privilege(r.oid, 'public', 'CREATE'))
		              FROM pg_catalog.pg_roles r WHERE pg_catalog.pg_has_role($1, r.oid, 'MEMBER')), false),
		  EXISTS (SELECT 1 FROM acl WHERE grantee = 0),
		  EXISTS (SELECT 1 FROM acl, me WHERE acl.grantee = me.oid),
		  COALESCE((SELECT array_agg(pg_catalog.pg_get_userbyid(acl.grantee) ORDER BY 1) FROM acl, me
		             WHERE acl.grantee <> 0 AND acl.grantee <> me.oid
		               AND pg_catalog.pg_has_role($1, acl.grantee, 'MEMBER')), '{}'),
		  (SELECT pg_catalog.pg_has_role($1, nspowner, 'MEMBER') FROM n),
		  (SELECT pg_catalog.pg_get_userbyid(nspowner) FROM n)`, role).
		Scan(&pc.can, &pc.public, &pc.direct, &pc.groups, &pc.owner, &pc.nspOwner)
	if err != nil {
		return pc, fmt.Errorf("check %s's rights on schema public: %w", role, err)
	}
	return pc, nil
}

// ownsCreate says whether owner can create in public other than through
// PUBLIC: as the schema's owner, or by a grant to it or a role whose rights
// it inherits. Without that, revoking PUBLIC's grant would leave the next
// migrate unable to make its tables.
func ownsCreate(ctx context.Context, q queryRower, owner string) (bool, error) {
	var ok bool
	err := q.QueryRow(ctx, `
		WITH n AS (SELECT nspowner, nspacl FROM pg_catalog.pg_namespace WHERE nspname = 'public')
		SELECT (SELECT pg_catalog.pg_has_role($1, nspowner, 'USAGE') FROM n)
		    OR EXISTS (SELECT 1 FROM n, aclexplode(n.nspacl) a
		               WHERE a.privilege_type = 'CREATE' AND a.grantee <> 0
		                 AND pg_catalog.pg_has_role($1, a.grantee, 'USAGE'))`, owner).Scan(&ok)
	if err != nil {
		return false, fmt.Errorf("check %s's rights on schema public: %w", owner, err)
	}
	return ok, nil
}

// routes names how the role can create, for a refusal.
func (pc publicCreate) routes() string {
	var r []string
	if pc.public {
		r = append(r, "PUBLIC's grant, which every role has before Postgres 15")
	}
	if pc.direct {
		r = append(r, "a grant to the role itself")
	}
	for _, g := range pc.groups {
		r = append(r, "a grant to "+g+", a role it belongs to")
	}
	if pc.owner {
		r = append(r, "owning the schema, directly or as a member of its owner "+pc.nspOwner+
			" (on Postgres 15 and later that is pg_database_owner, so owning the database is enough)")
	}
	if len(r) == 0 {
		r = append(r, "a route this check could not name")
	}
	return strings.Join(r, "; ")
}

// fix is what to run, in order, as the schema's owner or a superuser.
func (pc publicCreate) fix(runtime, owner string, ownerHasOwn bool) string {
	rt, ow := pgx.Identifier{runtime}.Sanitize(), pgx.Identifier{owner}.Sanitize()
	var s []string
	if pc.public {
		if !ownerHasOwn {
			s = append(s, "GRANT CREATE ON SCHEMA public TO "+ow+";")
		}
		s = append(s, "REVOKE CREATE ON SCHEMA public FROM PUBLIC;")
	}
	if pc.direct {
		s = append(s, "REVOKE CREATE ON SCHEMA public FROM "+rt+";")
	}
	for _, g := range pc.groups {
		s = append(s, "REVOKE CREATE ON SCHEMA public FROM "+pgx.Identifier{g}.Sanitize()+"; (or take "+runtime+" out of "+g+")")
	}
	if pc.owner {
		s = append(s, "give schema public, and the database, to a role "+runtime+" is not a member of "+
			"(ALTER SCHEMA public OWNER TO "+ow+"; ALTER DATABASE … OWNER TO …);")
	}
	return strings.Join(s, " ")
}

// publicCreateRefusal is the one message both the migrate and a runtime
// server give.
func publicCreateRefusal(pc publicCreate, runtime, owner string, ownerHasOwn bool) string {
	return fmt.Sprintf("the runtime role %q can create objects in schema public, through %s, and could use that "+
		"to stand in for what the record's triggers call. As the schema's owner or a superuser (on Postgres 13 and "+
		"14 the schema usually belongs to the superuser, so this is a superuser step), run: %s "+
		"Then run abhed migrate again (see docs/guide/02-configuration.md)",
		runtime, pc.routes(), pc.fix(runtime, owner, ownerHasOwn))
}

// closePublicCreate is the migrate's part, run as the owner under the
// provision lock. Where the owner may (it owns schema public, or belongs to
// the role that does), it gives itself CREATE if it had it only through
// PUBLIC and then revokes PUBLIC's; it never revokes PUBLIC's grant while
// the owner would be left without CREATE. Whatever route remains refuses
// the migrate with the complete fix.
func closePublicCreate(ctx context.Context, conn interface {
	queryRower
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}, owner, runtime string) error {
	pc, err := publicCreateOf(ctx, conn, runtime)
	if err != nil || !pc.can {
		return err
	}
	own, err := ownsCreate(ctx, conn, owner)
	if err != nil {
		return err
	}
	if pc.public {
		if !own {
			_, _ = conn.Exec(ctx, `GRANT CREATE ON SCHEMA public TO `+pgx.Identifier{owner}.Sanitize())
			if own, err = ownsCreate(ctx, conn, owner); err != nil {
				return err
			}
		}
		if own {
			_, _ = conn.Exec(ctx, `REVOKE CREATE ON SCHEMA public FROM PUBLIC`)
		}
	}
	if pc, err = publicCreateOf(ctx, conn, runtime); err != nil || !pc.can {
		return err
	}
	return fmt.Errorf("provision: %s", publicCreateRefusal(pc, runtime, owner, own))
}

// refusePublicCreate is a runtime server's part: it refuses to start while
// its role can create in schema public, with the same fix the migrate gives.
func refusePublicCreate(ctx context.Context, q queryRower) error {
	var me, owner string
	if err := q.QueryRow(ctx, `SELECT current_user,
		COALESCE((SELECT pg_catalog.pg_get_userbyid(relowner) FROM pg_catalog.pg_class
		          WHERE oid = pg_catalog.to_regclass('public.events')), '')`).Scan(&me, &owner); err != nil {
		return fmt.Errorf("check the connected role's rights on schema public: %w", err)
	}
	pc, err := publicCreateOf(ctx, q, me)
	if err != nil || !pc.can {
		return err
	}
	own := false
	if owner != "" {
		if own, err = ownsCreate(ctx, q, owner); err != nil {
			return err
		}
	} else {
		owner = "<owner role>"
	}
	return fmt.Errorf("refusing to start: %s", publicCreateRefusal(pc, me, owner, own))
}
