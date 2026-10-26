package pgstore

// v46DDL ports migrateV45ToV46: the one-row event low watermark, seeded so
// every seq below the lowest surviving event reads as pruned.
const v46DDL = `
CREATE TABLE IF NOT EXISTS event_low_watermark (
	id  BIGINT PRIMARY KEY CHECK (id = 1),
	seq BIGINT NOT NULL
);

INSERT INTO event_low_watermark (id, seq)
	SELECT 1, COALESCE(MIN(seq) - 1, 0) FROM events
ON CONFLICT DO NOTHING;
`
