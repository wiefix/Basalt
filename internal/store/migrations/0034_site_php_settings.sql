-- The site's PHP options (php.Settings as JSON; empty object: defaults).
ALTER TABLE sites ADD COLUMN php_settings TEXT NOT NULL DEFAULT '{}';
