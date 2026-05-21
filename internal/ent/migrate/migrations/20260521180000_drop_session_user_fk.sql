-- Drop FK constraint on pos_device_sessions.user_id so that terminal PIN staff
-- (stored in staff_members, not users) can open shift sessions without requiring
-- a corresponding record in the users table.
ALTER TABLE "pos_device_sessions" DROP CONSTRAINT IF EXISTS "pos_device_sessions_users_pos_sessions";
