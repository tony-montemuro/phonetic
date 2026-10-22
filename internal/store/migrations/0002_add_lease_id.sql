-- lease_id identifies a lease independently of its expiry. A random value is
-- assigned at claim time; lease-guarded writes compare-and-set on
-- (id, lease_id, status). Heartbeats extend only locked_until, so they can
-- never invalidate the holder's own token, and a reissued expiry timestamp
-- can't impersonate an old lease.
ALTER TABLE articles ADD COLUMN lease_id INTEGER;
