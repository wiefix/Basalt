-- The local part (in the same domain) that receives mail for unknown
-- addresses of a mail domain; empty: none.
ALTER TABLE mail_domains ADD COLUMN catch_all TEXT NOT NULL DEFAULT '';
