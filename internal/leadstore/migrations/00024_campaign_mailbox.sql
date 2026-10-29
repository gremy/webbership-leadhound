-- +goose Up
-- Mailboxes assigned per campaign, and where a mailbox's domain sends a visitor.
--
-- campaign_mailbox is append-only (UPDATE revoked from the app role, as on send_pause): every
-- assign and unassign is a row, the latest row (by id) of a campaign and mailbox pair is the
-- assignment, and no row means not assigned. A campaign sends only from its assigned
-- mailboxes. That the mailbox and the campaign belong to the same account is enforced by the
-- store (no cross-table check exists to mirror).
--
-- Seeded so nothing stops at deploy: each mailbox that is not retired and whose live
-- (non-shadow batch) messages all belong to one campaign is assigned to that campaign, as
-- system. A selftest message (its lead's kind is selftest) does not count: a selftest batch is
-- live and carries campaign 1 whatever the mailbox is for. A mailbox with no live message
-- other than selftests, or with live messages of two campaigns, gets no row.
--
-- mailbox_check.landing_url: the final URL of https://<mailbox domain>/ after redirects, NULL
-- until the next check (or when the site did not answer). It joins mailbox_check's UPDATE-able
-- result columns (leadstore.Migrate and deploy/sql/grants.sql).
SET LOCAL lock_timeout = '5s';

CREATE TABLE campaign_mailbox (
  id bigserial PRIMARY KEY,
  campaign_id bigint NOT NULL REFERENCES campaign(id),
  mailbox_id bigint NOT NULL REFERENCES sender_mailbox(id),
  assigned boolean NOT NULL,
  actor text NOT NULL,                -- as send_pause.actor
  at timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT campaign_mailbox_actor_check CHECK (actor IN ('gremy', 'system'))
);
CREATE INDEX campaign_mailbox_pair_idx ON campaign_mailbox (campaign_id, mailbox_id, id DESC);

INSERT INTO campaign_mailbox (campaign_id, mailbox_id, assigned, actor)
SELECT min(b.campaign_id), mb.id, true, 'system'
FROM sender_mailbox mb
JOIN outreach_message m ON m.mailbox_id = mb.id
JOIN outreach_batch b ON b.id = m.batch_id AND NOT b.shadow
JOIN lead l ON l.id = m.lead_id AND l.kind <> 'selftest'
WHERE (SELECT s.state FROM mailbox_state s WHERE s.mailbox_id = mb.id ORDER BY s.id DESC LIMIT 1)
      IS DISTINCT FROM 'retired'
GROUP BY mb.id
HAVING count(DISTINCT b.campaign_id) = 1
   -- the one campaign is the mailbox's own account's
   AND bool_and(EXISTS (SELECT 1 FROM campaign c WHERE c.id = b.campaign_id AND c.account_id = mb.account_id))
ORDER BY mb.id;

ALTER TABLE mailbox_check ADD COLUMN landing_url text;

-- +goose Down
-- Runs as the owner only; the app role cannot drop. Every assignment made since goes with it.
SET LOCAL lock_timeout = '5s';
ALTER TABLE mailbox_check DROP COLUMN landing_url;
DROP TABLE campaign_mailbox;
