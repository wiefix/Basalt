-- The background processes of a Laravel site (php.Laravel as JSON:
-- scheduler, queue workers; empty object: none).
ALTER TABLE sites ADD COLUMN laravel TEXT NOT NULL DEFAULT '{}';
