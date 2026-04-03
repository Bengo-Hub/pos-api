-- Fix outbox_events table to match shared-events@v0.2.0 schema.
-- Add missing columns: aggregate_type, aggregate_id, attempts, last_attempt_at, published_at, error_message.
-- Migrate status values from lowercase to uppercase.
-- Drop legacy columns: metadata, retry_count, processed_at, last_error.

-- Add new columns required by shared-events v0.2.0
ALTER TABLE "outbox_events" ADD COLUMN "aggregate_type" character varying NOT NULL DEFAULT '';
ALTER TABLE "outbox_events" ADD COLUMN "aggregate_id" character varying NOT NULL DEFAULT '';
ALTER TABLE "outbox_events" ADD COLUMN "attempts" bigint NOT NULL DEFAULT 0;
ALTER TABLE "outbox_events" ADD COLUMN "last_attempt_at" timestamptz NULL;
ALTER TABLE "outbox_events" ADD COLUMN "published_at" timestamptz NULL;
ALTER TABLE "outbox_events" ADD COLUMN "error_message" text NULL;

-- Migrate existing data: copy retry_count -> attempts, last_error -> error_message, processed_at -> published_at
UPDATE "outbox_events" SET "attempts" = "retry_count";
UPDATE "outbox_events" SET "error_message" = "last_error" WHERE "last_error" IS NOT NULL;
UPDATE "outbox_events" SET "published_at" = "processed_at" WHERE "processed_at" IS NOT NULL;

-- Migrate status values from lowercase to uppercase
UPDATE "outbox_events" SET "status" = 'PENDING' WHERE "status" = 'pending';
UPDATE "outbox_events" SET "status" = 'PUBLISHED' WHERE "status" = 'published';
UPDATE "outbox_events" SET "status" = 'FAILED' WHERE "status" = 'failed';

-- Update default for status column
ALTER TABLE "outbox_events" ALTER COLUMN "status" SET DEFAULT 'PENDING';

-- Drop legacy columns
ALTER TABLE "outbox_events" DROP COLUMN IF EXISTS "metadata";
ALTER TABLE "outbox_events" DROP COLUMN IF EXISTS "retry_count";
ALTER TABLE "outbox_events" DROP COLUMN IF EXISTS "processed_at";
ALTER TABLE "outbox_events" DROP COLUMN IF EXISTS "last_error";

-- Drop old indexes and create new ones
DROP INDEX IF EXISTS "outboxevent_status_created_at";
DROP INDEX IF EXISTS "outboxevent_tenant_id";
CREATE INDEX IF NOT EXISTS "outboxevent_status" ON "outbox_events" ("status");
CREATE INDEX IF NOT EXISTS "outboxevent_created_at" ON "outbox_events" ("created_at");
CREATE INDEX IF NOT EXISTS "outboxevent_tenant_id_status" ON "outbox_events" ("tenant_id", "status");
