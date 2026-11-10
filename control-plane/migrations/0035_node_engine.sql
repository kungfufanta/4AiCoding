-- 0035 (M8): the executor engine of a fleet member. The docker executor
-- (M7b) and the kubernetes executor (M8, PLAN §2 k3s B2) coexist: the
-- control plane places on either, and the console shows which engine
-- each node runs. "docker" is the default for every pre-M8 row.
ALTER TABLE nodes ADD COLUMN engine TEXT NOT NULL DEFAULT 'docker';
