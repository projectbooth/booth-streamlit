-- Build step 3 (lifecycle).
--
-- gate_bearer: the per-app secret the backend presents to the app pod's gate container (design
-- note (b)). The backend writes it into a Secret for the gate and keeps it here for itself, because
-- its Role may create and delete Secrets but never read them.
--
-- suspended: idle shutdown stopped this app's container without changing the owner's choice
-- (desired_state stays 'running'). Any member allowed to open it wakes it; only an owner's Stop
-- makes it stay down (ADR 0105).
ALTER TABLE apps ADD COLUMN gate_bearer TEXT NOT NULL DEFAULT '';
ALTER TABLE apps ADD COLUMN suspended   BOOLEAN NOT NULL DEFAULT FALSE;

-- Apps created before this migration get a bearer now, so no code path ever sees an empty one.
-- gen_random_uuid() is built in since PostgreSQL 13; two of them give 244 random bits.
UPDATE apps SET gate_bearer = replace(gen_random_uuid()::text || gen_random_uuid()::text, '-', '') WHERE gate_bearer = '';
