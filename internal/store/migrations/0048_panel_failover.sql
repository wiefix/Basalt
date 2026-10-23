-- Where agents reach each standby panel once it takes over (its agent
-- port, as the standby reports it); agents learn these addresses.
ALTER TABLE standby_panels ADD COLUMN agent_url TEXT NOT NULL DEFAULT '';
