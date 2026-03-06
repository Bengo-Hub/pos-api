# RBAC and Seed (pos-api)

**Last updated**: March 2026

## RBAC: auth-api JWT (no local Permission/Role schema today)

pos-api does **not** currently maintain its own Role or Permission database tables. Authorization is enforced via **auth-api JWT** using `shared-auth-client`: protected routes require a valid Bearer token; user identity and tenant context come from JWT claims. Frontends call auth-api `GET /me` for roles and permissions (e.g. TanStack Query with TTL).

When defining permissions in **auth-api** for POS consumers (or when adding a local Role/Permission schema to pos-api later), use the standard **eight-action set** per resource:

| Action       | Description                            |
|-------------|----------------------------------------|
| `add`       | Create new records                     |
| `read`      | View any record                        |
| `read_own`  | View own/tenant-scoped records only    |
| `change`    | Update any record                      |
| `change_own`| Update own/tenant-scoped records only  |
| `delete`    | Delete/cancel records                  |
| `manage`    | Full management (all of the above)    |
| `manage_own`| Full management of own scope only      |

**POS resources** to cover with these actions (for auth-api seed or future pos-api seed):

- **orders** — POS orders, tickets, order lines
- **products** — catalog/pricebook (read cache from inventory; local overrides)
- **drawer** — cash drawer sessions, float, open/close, counts
- **payments** — tender capture, refunds (reference treasury)
- **promotions** — promotions, rules, applications
- **gift_cards** — gift card issuance and redemption
- **tables** — table layout and assignments (dine-in)
- **bar_tabs** — bar tab management
- **reports** — sales and cash reports, exports
- **settings** — outlet/config and integration settings

Example permission codes: `orders:read`, `orders:add`, `drawer:manage`, `products:read`, `promotions:change`, etc.

## Seed: no cmd/seed today

- pos-api has **no** `cmd/seed` binary at present. No migration files are added manually; use Ent (or existing) migration tooling when schema is introduced.
- **Core data** when schema exists: seed tenants/outlets from auth-api or tenant-sync events; seed default config (timezone, tax, tenders) per outlet as needed. Reuse patterns from **ordering-backend** (`cmd/seed`) or **auth-api** (`cmd/seed`) for tenant and role/permission seeding if pos-api later adds local Role/Permission tables.
- **Default config**: Document service-level defaults in `config/example.env`.

## References

- Auth-api seed: `auth-service/auth-api/cmd/seed` (resources include `orders`, `menu`; extend with POS resources as needed).
- Ordering-backend seed: `ordering-service/ordering-backend/cmd/seed` (permissions, roles, tenant, catalog; uses the same eight-action set for orders/catalog).
- Workspace rule: `.cursor/rules/uniformity-rule.mdc` (RBAC and seed alignment).

## DevOps file locations (do not modify in this doc)

- **pos-api**: `devops-k8s/apps/pos-api/` — `values.yaml`, `app.yaml`, and related Helm/ArgoCD config.
- **pos-ui**: `devops-k8s/apps/pos-ui/` — `values.yaml`, `app.yaml`, and related Helm/ArgoCD config.
