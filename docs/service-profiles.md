# Services outlets: service profiles and job orders

A `services` outlet runs one service profile, the kind of service business it is. The profile
decides which workflow the till runs, which of the tenant's catalog services it shows, and what
reception captures for each sale. The tenant defines its own services in inventory; staff and
customers only pick from that catalog and add instructions and reference media.

## Profiles

The registry lives in `internal/modules/outletpolicy/service_profiles.go` and is served at
`GET /{tenant}/pos/service-profiles`. pos-ui reads it from there; nothing is duplicated
client-side. An admin picks the profile under Settings, Modules
(`PATCH /{tenant}/pos/settings/service-profile`). It is stored in
`outlet_settings.metadata.service_profile` together with the default job deposit
(`job_deposit_percent`).

| Profile | Workflow | Inventory service type |
|---|---|---|
| Printing & Branding | job | PRINTING_SERVICE |
| Salon & Barbershop | appointment | SALON_SERVICE |
| Nail Parlour | appointment | NAIL_SERVICE, SALON_SERVICE |
| Spa & Wellness | appointment | SPA_SERVICE, SALON_SERVICE |
| Garage & Auto Service | job | AUTO_SERVICE |
| Car Wash & Detailing | queue | AUTO_SERVICE |
| Laundry & Dry Cleaning | job | LAUNDRY_SERVICE |
| Tailoring & Fashion Design | job | TAILORING_SERVICE |
| Phone & Electronics Repair | job (repair module) | REPAIR_SERVICE |
| General Professional Services | job | any |

Every profile also accepts `PROFESSIONAL_SERVICE` items, and untagged items (still on the
inventory `RETAIL` default) so an existing catalog keeps working until it is tagged. A profile
that sells goods (a print shop's paper, a salon's hair products) also shows GOODS in merchandise
categories; food and ingredient categories never show.

Picking a job profile turns on the production board (`enable_kds`) and creates the profile's
default station when the outlet has none. Picking an appointment profile turns on appointments.
Nothing an admin already configured is removed.

## Job orders

A job workflow outlet books every sale as order subtype `service_job`.

1. **Reception** picks the catalog services, attaches the customer (a phone number is required)
   and fills the job sheet: ready-by time, instructions, design from scratch, reference links
   (Google Drive, YouTube, any URL) and uploads, and a spec sheet per item from the profile's
   spec fields. "Create Job" places the order unpaid.
2. **Deposit.** The job-created dialog suggests the outlet's default deposit (70% for printing)
   and takes it through the normal payment modal. A deposit is an ordinary partial payment, so
   the order stays open with a balance.
3. **Production board** (`/production` in pos-ui). A `service_job` order opens like a kitchen
   order, so its lines become production tickets with their spec sheets. The board shows one
   column per production stage and one card per job, overdue jobs first. Staff move a job to the
   next stage, record the customer's proof decision and open the attachments.
4. **Finished.** Reaching the last stage (Ready for collection) clears the job's tickets and moves
   an unpaid or part-paid job to `pending_payment`, the same hand-off a served kitchen order
   makes.
5. **Collection.** On the Orders page the cashier collects the outstanding balance (not the full
   total), the final receipt prints with the amount paid, and "Mark collected" closes the job.

A job paid in full up front stays on the board until it is finished; payment completion does not
clear a job's production tickets. A collected job is locked.

### Job header

Stored in `pos_orders.metadata.job`, validated by `orders.NormalizeNewJob` on create and
`orders.ApplyJobUpdate` on `PATCH /{tenant}/pos/orders/{id}/job`:

| Key | Meaning |
|---|---|
| `due_at` | RFC 3339 ready-by time |
| `brief` | instructions, 4,000 characters max |
| `design_from_scratch` | the business designs the artwork |
| `attachments` | `[{kind: link or file, url, label}]`, 10 links and 5 files max |
| `stage`, `stage_history` | current production stage and who moved it when |
| `proof_status` | `none`, `sent`, `approved`, `changes_requested` |
| `collected_at` | when the customer collected the job |

Per-line spec sheets are in `pos_order_lines.metadata.job_specs` and travel onto the production
ticket items.

### Attachments

`POST /{tenant}/pos/orders/job-attachments` stores one PNG, JPEG, WebP or PDF of at most 512 KB on
the pos-api media volume (`{MEDIA_ROOT}/{tenant}/job-attachments/`) and returns its `/media/...`
URL. The type is sniffed from the file bytes. The terminal checks size and count before
uploading; anything bigger is shared as a link. The same storage helper (`storeTenantMedia`)
serves screensaver, damage evidence and lost-and-found uploads.

### Dashboard and queries

`GET /{tenant}/pos/jobs/summary` returns jobs in production, ready for collection, due today,
overdue and awaiting proof, plus deposits held and the balance still to collect, for the active
outlet. It reads only job rows through the partial index `posorder_service_jobs`
(`tenant_id, outlet_id, status, created_at WHERE order_subtype = 'service_job'`), so it stays the
size of the job book as sales history grows. Paid jobs never marked collected count for 90 days.
The Orders list accepts `order_subtype=service_job`.

## Gating

The production board uses the KDS routes, now open to `services` outlets. The food `kds` plan
feature gates the kitchen display only; a services board runs on the outlet's `enable_kds`
toggle. `RequireUseCase` compares the normalized use case, so an outlet stored as "salon" reaches
the services routes.

## Demo

codevertex-demo has "Demo Print & Branding" (code PRT, profile printing_branding, one Production
station) and twelve PRINTING_SERVICE catalog items seeded by inventory-api. "Demo Beauty &
Wellness" runs the salon_barber profile.
