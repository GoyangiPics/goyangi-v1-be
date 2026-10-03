package hooks

import (
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/hook"
)

// requireAdmin gates a route on `users.isAdmin`.
//
// Bind it AFTER apis.RequireAuth so e.Auth is guaranteed non-nil:
//
//	e.Router.POST(path, h).Bind(apis.RequireAuth("users"), requireAdmin())
//
// isAdmin is writable only by a superuser (no API rule permits it — see
// pb_schema.json), which is what makes this meaningful. Deliberately no
// "admins can promote admins" path: that turns one compromised admin session
// into permanent root.
func requireAdmin() *hook.Handler[*core.RequestEvent] {
	return &hook.Handler[*core.RequestEvent]{
		Id: "goyangiRequireAdmin",
		Func: func(e *core.RequestEvent) error {
			if e.Auth == nil || !e.Auth.GetBool("isAdmin") {
				return e.ForbiddenError("Admin only.", nil)
			}
			return e.Next()
		},
	}
}
