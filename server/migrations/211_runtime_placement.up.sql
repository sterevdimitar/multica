-- 211_runtime_placement.up.sql
--
-- Runtime placement (dev-command-center design
-- 2026-09-20-runtime-placement): each webhook runtime carries a cap, a
-- position in the fallback order and a measured availability; each agent
-- carries the set of runtimes it may fail over to.
--
--   max_concurrent_tasks     runs at once. NULL = no limit (the pre-migration
--                            behaviour); 0 = out of the rotation, the manual
--                            switch for a runtime that must not be used.
--   dispatch_order           position in the fallback order; lower first,
--                            ties by created_at. Written only by
--                            PUT /api/runtimes/order.
--   down_until               the runtime is unavailable until this instant.
--                            NULL or past = available. Exactly three writers:
--                            the availability probe, a 409 from the receiver,
--                            and a dispatch_timeout failure.
--   down_reason              why, for the health cell.
--   availability_checked_at  when the probe last answered.
--   agent.fallback_runtime_ids
--                            the runtimes this agent may fail over to. Written
--                            by the pipeline reconciler from allowed_runtimes;
--                            never by the UI. '{}' = pinned: the agent waits
--                            for its own runtime and never fails over.
--
-- Additive; the defaults reproduce today's behaviour exactly: no caps, one
-- order (registration order), no fallbacks, nothing down.
ALTER TABLE agent_runtime
    ADD COLUMN max_concurrent_tasks INT NULL CHECK (max_concurrent_tasks >= 0),
    ADD COLUMN dispatch_order INT NOT NULL DEFAULT 0,
    ADD COLUMN down_until TIMESTAMPTZ NULL,
    ADD COLUMN down_reason TEXT NULL,
    ADD COLUMN availability_checked_at TIMESTAMPTZ NULL;

ALTER TABLE agent
    ADD COLUMN fallback_runtime_ids UUID[] NOT NULL DEFAULT '{}';
