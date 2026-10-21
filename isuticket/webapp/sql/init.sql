-- ベンチマーク走行前の初期化。
-- 初期データは isuticket_init データベースに同じ内容が入っている（プロビジョニング時に作成）。
-- 走行中に追加された行を消し、走行中に変更され得る行だけを初期状態に戻す。

TRUNCATE TABLE sessions;
DELETE FROM settings;
INSERT INTO settings (name, value) VALUES ('resale_lock', '');

SET @m = (SELECT MAX(id) FROM isuticket_init.users);
DELETE FROM users WHERE id > @m;

SET @m = (SELECT MAX(id) FROM isuticket_init.events);
DELETE FROM events WHERE id > @m;
DELETE FROM event_ranks WHERE event_id > @m;

SET @m = (SELECT MAX(id) FROM isuticket_init.event_seats);
DELETE FROM event_seats WHERE id > @m;

SET @m = (SELECT MAX(id) FROM isuticket_init.orders);
DELETE FROM orders WHERE id > @m;

SET @m = (SELECT MAX(id) FROM isuticket_init.tickets);
DELETE FROM tickets WHERE id > @m;

SET @m = (SELECT MAX(id) FROM isuticket_init.lottery_entries);
DELETE FROM lottery_entries WHERE id > @m;

-- 走行中に購入・リセールされ得るのは販売中・販売前の公演（id 401〜）の座席とチケットだけ
UPDATE event_seats s JOIN isuticket_init.event_seats i ON i.id = s.id
SET s.status = i.status, s.ticket_id = i.ticket_id, s.updated_at = i.updated_at
WHERE i.event_id > 400;

UPDATE tickets t JOIN isuticket_init.tickets i ON i.id = t.id
SET t.user_id = i.user_id, t.serial = i.serial, t.status = i.status, t.used_at = i.used_at
WHERE i.event_id > 400;

TRUNCATE TABLE resale_listings;
INSERT INTO resale_listings SELECT * FROM isuticket_init.resale_listings;
