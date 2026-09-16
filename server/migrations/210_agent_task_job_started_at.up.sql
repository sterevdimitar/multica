-- 210_agent_task_job_started_at.up.sql
--
-- job_started_at: the instant the runner's job began executing on a runner —
-- the first step of the GitHub job, before checkout. Written by /start from
-- the startup_ms the runner reports (now() - startup_ms), never by the
-- runner's own clock, so it is always on this server's timeline and never
-- later than started_at.
--
-- It is the left end of the popover's wall-time span. dispatched_at cannot
-- be: everything between the webhook and the runner picking the job up is
-- the GitHub queue, during which nothing is executing, and a total that
-- counts it grows with the queue rather than with the pipeline's own work.
-- NULL for a task that never reached a runner (cancelled while queued, a
-- dispatch timeout), and for one started by a caller that reports no
-- startup — the daemon, and any runner that predates the field.
ALTER TABLE agent_task_queue
    ADD COLUMN job_started_at TIMESTAMPTZ;
