// Package main: row_policy.go implements Row Level Security Lite — at most
// one policy per table, naming the column whose value must match the acting
// username for a non-Admin session to see/create/edit/delete a row.
//
// Scope boundary (this is the "Lite" in the name, stated here and in the
// Admin UI, not hidden): policies are enforced only on DataDock's own
// generated SQL — table browse (tableRowsSorted), record CRUD (insertRecord/
// updateRecord/deleteRecord/getRecord in db.go), and export
// (exportTableHandler in handlers.go). They do NOT rewrite or restrict the
// free-form SQL editor (/query, /api/query): safely injecting a WHERE clause
// into arbitrary user-authored SQL (subqueries, joins, UNIONs, CTEs) is a
// fundamentally harder and riskier problem than this feature attempts to
// solve. An operator relying on row policies for real isolation should scope
// SQL-editor write access accordingly (e.g. Admin-only) for protected tables.
//
// Persistence follows the same delete-then-insert-by-name upsert idiom as
// match_config.go, against the local tinySQL connection like every other
// piece of DataDock's own metadata.
package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// rowPoliciesTable stores at most one row policy per table name.
const rowPoliciesTable = "__datadock_row_policies"

// RowPolicy is one table's row-level-security-lite rule: OwnerColumn must
// equal the acting username for a non-Admin session to see/touch a row.
type RowPolicy struct {
	TableName   string
	OwnerColumn string
	UpdatedAt   time.Time
}

// saveRowPolicy creates or overwrites (by table name) a row policy.
func (a *App) saveRowPolicy(ctx context.Context, p RowPolicy) error {
	table := strings.TrimSpace(p.TableName)
	column := strings.TrimSpace(p.OwnerColumn)
	if table == "" {
		return fmt.Errorf("table name is required")
	}
	if column == "" {
		return fmt.Errorf("owner column is required")
	}
	if err := a.ensureRowPoliciesTable(ctx); err != nil {
		return err
	}
	updatedAt := time.Now().UTC()
	conn := a.localTinySQLConn()
	if _, err := a.execConn(ctx, conn, "row_policy.delete", rowPoliciesDeleteSQL, table); err != nil {
		return err
	}
	_, err := a.execConn(ctx, conn, "row_policy.insert", rowPoliciesInsertSQL, table, column, updatedAt.Format(time.RFC3339))
	return err
}

// loadRowPolicy looks up the active policy for a table, if any. ok is false
// (with a nil error) when the table has no policy — the common case, and
// the fast path every policy-aware query builder call site takes.
func (a *App) loadRowPolicy(ctx context.Context, table string) (RowPolicy, bool, error) {
	table = strings.TrimSpace(table)
	if table == "" {
		return RowPolicy{}, false, nil
	}
	if err := a.ensureRowPoliciesTable(ctx); err != nil {
		return RowPolicy{}, false, err
	}
	rows, err := a.queryConn(ctx, a.localTinySQLConn(), "row_policy.load", rowPoliciesSelectOneSQL, table)
	if err != nil {
		return RowPolicy{}, false, err
	}
	defer rows.Close()
	if !rows.Next() {
		return RowPolicy{}, false, nil
	}
	var p RowPolicy
	var updatedAt string
	if err := rows.Scan(&p.TableName, &p.OwnerColumn, &updatedAt); err != nil {
		return RowPolicy{}, false, err
	}
	p.UpdatedAt, _ = time.Parse(time.RFC3339, updatedAt)
	return p, true, nil
}

// listRowPolicies returns every active policy, sorted by table name, for the
// Admin Settings row-policy management section.
func (a *App) listRowPolicies(ctx context.Context) ([]RowPolicy, error) {
	if err := a.ensureRowPoliciesTable(ctx); err != nil {
		return nil, err
	}
	rows, err := a.queryConn(ctx, a.localTinySQLConn(), "row_policy.list", rowPoliciesSelectAllSQL)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RowPolicy
	for rows.Next() {
		var p RowPolicy
		var updatedAt string
		if err := rows.Scan(&p.TableName, &p.OwnerColumn, &updatedAt); err != nil {
			return nil, err
		}
		p.UpdatedAt, _ = time.Parse(time.RFC3339, updatedAt)
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].TableName) < strings.ToLower(out[j].TableName) })
	return out, nil
}

// deleteRowPolicy removes a table's policy, if any.
func (a *App) deleteRowPolicy(ctx context.Context, table string) error {
	table = strings.TrimSpace(table)
	if table == "" {
		return fmt.Errorf("table name is required")
	}
	if err := a.ensureRowPoliciesTable(ctx); err != nil {
		return err
	}
	_, err := a.execConn(ctx, a.localTinySQLConn(), "row_policy.delete", rowPoliciesDeleteSQL, table)
	return err
}

func (a *App) ensureRowPoliciesTable(ctx context.Context) error {
	_, err := a.execConn(ctx, a.localTinySQLConn(), "row_policy.ensure_table", rowPoliciesEnsureTableSQL)
	if err == nil {
		return nil
	}
	if strings.Contains(strings.ToLower(err.Error()), "already exists") {
		return nil
	}
	return fmt.Errorf("ensure row policies table: %w", err)
}

// rowPolicyOwnerForSession resolves the owner-column value a row-policy
// check/stamp should use for the current session: the signed-in username,
// or "" for an anonymous session (which then never matches any row,
// correctly denying access to a policy-protected table rather than silently
// exempting anonymous sessions from the policy).
func (a *App) rowPolicyOwnerForSession(ctx context.Context) string {
	username, _, _ := a.currentSessionUser(sessionIDFromContext(ctx))
	return username
}

// rowPolicyAppliesToSession reports whether table's row policy (if any)
// should be enforced for the current session: there must be an active
// policy, and the session must not be an Admin (Admins bypass row policies
// entirely, matching the existing "admin sees everything" pattern used for
// DataDock's own system tables). Returns the policy and true when it should
// be enforced, so a call site can do:
//
//	if policy, enforce, err := a.rowPolicyAppliesToSession(ctx, table); enforce { ... }
func (a *App) rowPolicyAppliesToSession(ctx context.Context, table string) (RowPolicy, bool, error) {
	policy, found, err := a.loadRowPolicy(ctx, table)
	if err != nil || !found {
		return RowPolicy{}, false, err
	}
	if a.isAdminAuthenticated(sessionIDFromContext(ctx)) {
		return policy, false, nil
	}
	return policy, true, nil
}

// rowPolicyOwnerColumnAndArg is the single call every policy-aware query
// builder call site in db.go/handlers.go uses: it resolves whether table's
// row policy applies to the current session and, if so, returns the owner
// column name and the value (acting username) to bind against it. Returns
// ("", nil, nil) when no policy applies — the common case, and the signal
// callers use to build their query exactly as before (see the "ownerColumn
// != \"\"" checks throughout queries.go's record/table query builders).
func (a *App) rowPolicyOwnerColumnAndArg(ctx context.Context, table string) (ownerColumn string, ownerValue string, err error) {
	policy, enforce, err := a.rowPolicyAppliesToSession(ctx, table)
	if err != nil || !enforce {
		return "", "", err
	}
	return policy.OwnerColumn, a.rowPolicyOwnerForSession(ctx), nil
}

// cloneStringMapWithOverride copies m and sets key to value in the copy,
// leaving the caller's original map untouched — used by insertRecord to
// force-stamp a row policy's owner column without mutating the record-form
// values map the caller may still reference (e.g. to re-render on error).
func cloneStringMapWithOverride(m map[string]string, key, value string) map[string]string {
	out := make(map[string]string, len(m)+1)
	for k, v := range m {
		out[k] = v
	}
	out[key] = value
	return out
}
