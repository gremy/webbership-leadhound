-- +goose Up
-- Every outreach message, mailbox event and mailbox cursor names its sending mailbox, and
-- the event and cursor keys carry it, so two mailboxes that both read INBOX keep their own
-- cursors and their own events (00007 added the columns as nullable).
--
-- Rows from before mailboxes (no mailbox_id) are given one placeholder mailbox, retired by
-- the system, which nothing sends through or reads: legacy@legacy.invalid on the reserved
-- .invalid TLD, with no password (password_sealed empty, sealed_by ''). A database with no
-- such row gets no placeholder.
INSERT INTO sender_mailbox (address, domain, login, smtp_addr, imap_addr, dkim_selector, password_sealed, sealed_by)
SELECT 'legacy@legacy.invalid', 'legacy.invalid', 'legacy@legacy.invalid', 'legacy.invalid:465', 'legacy.invalid:993', '', ''::bytea, ''
WHERE EXISTS (SELECT 1 FROM outreach_message WHERE mailbox_id IS NULL)
   OR EXISTS (SELECT 1 FROM mailbox_event WHERE mailbox_id IS NULL)
   OR EXISTS (SELECT 1 FROM mailbox_cursor WHERE mailbox_id IS NULL)
ON CONFLICT (address) DO NOTHING;

INSERT INTO mailbox_state (mailbox_id, state, reason, actor)
SELECT id, 'retired', 'rows from before mailboxes', 'system' FROM sender_mailbox
WHERE address = 'legacy@legacy.invalid'
  AND NOT EXISTS (SELECT 1 FROM mailbox_state s WHERE s.mailbox_id = sender_mailbox.id AND s.state = 'retired');

UPDATE outreach_message SET mailbox_id = (SELECT id FROM sender_mailbox WHERE address = 'legacy@legacy.invalid')
WHERE mailbox_id IS NULL;
UPDATE mailbox_event SET mailbox_id = (SELECT id FROM sender_mailbox WHERE address = 'legacy@legacy.invalid')
WHERE mailbox_id IS NULL;
UPDATE mailbox_cursor SET mailbox_id = (SELECT id FROM sender_mailbox WHERE address = 'legacy@legacy.invalid')
WHERE mailbox_id IS NULL;

ALTER TABLE outreach_message ALTER COLUMN mailbox_id SET NOT NULL;
ALTER TABLE mailbox_event ALTER COLUMN mailbox_id SET NOT NULL;
ALTER TABLE mailbox_cursor ALTER COLUMN mailbox_id SET NOT NULL;

-- UIDs are per folder of one mailbox: the event key and the cursor key lead with it.
ALTER TABLE mailbox_event DROP CONSTRAINT mailbox_event_mailbox_uid_key;
ALTER TABLE mailbox_event ADD CONSTRAINT mailbox_event_mailbox_uid_key UNIQUE (mailbox_id, mailbox, uidvalidity, uid);
ALTER TABLE mailbox_cursor DROP CONSTRAINT mailbox_cursor_pkey;
ALTER TABLE mailbox_cursor ADD CONSTRAINT mailbox_cursor_pkey PRIMARY KEY (mailbox_id, mailbox);

-- +goose Down
-- Runs as the owner only. The old keys name the folder alone, so a database where two
-- mailboxes recorded the same folder (and UID) cannot go back: the key restore fails and the
-- whole step rolls back, nothing changed.
ALTER TABLE mailbox_cursor DROP CONSTRAINT mailbox_cursor_pkey;
ALTER TABLE mailbox_cursor ADD CONSTRAINT mailbox_cursor_pkey PRIMARY KEY (mailbox);
ALTER TABLE mailbox_event DROP CONSTRAINT mailbox_event_mailbox_uid_key;
ALTER TABLE mailbox_event ADD CONSTRAINT mailbox_event_mailbox_uid_key UNIQUE (mailbox, uidvalidity, uid);

ALTER TABLE mailbox_cursor ALTER COLUMN mailbox_id DROP NOT NULL;
ALTER TABLE mailbox_event ALTER COLUMN mailbox_id DROP NOT NULL;
ALTER TABLE outreach_message ALTER COLUMN mailbox_id DROP NOT NULL;

-- The placeholder's rows have no mailbox again, and the placeholder goes.
UPDATE mailbox_cursor SET mailbox_id = NULL
WHERE mailbox_id = (SELECT id FROM sender_mailbox WHERE address = 'legacy@legacy.invalid');
UPDATE mailbox_event SET mailbox_id = NULL
WHERE mailbox_id = (SELECT id FROM sender_mailbox WHERE address = 'legacy@legacy.invalid');
UPDATE outreach_message SET mailbox_id = NULL
WHERE mailbox_id = (SELECT id FROM sender_mailbox WHERE address = 'legacy@legacy.invalid');
DELETE FROM mailbox_check
WHERE mailbox_id = (SELECT id FROM sender_mailbox WHERE address = 'legacy@legacy.invalid');
DELETE FROM mailbox_state
WHERE mailbox_id = (SELECT id FROM sender_mailbox WHERE address = 'legacy@legacy.invalid');
DELETE FROM sender_mailbox WHERE address = 'legacy@legacy.invalid';
