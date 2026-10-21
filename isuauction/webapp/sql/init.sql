-- ベンチマーク走行前の初期化。
-- 初期データは isuauction_init データベースに同じ内容が入っている（プロビジョニング時に作成）。
-- 走行中に追加された行を消し、変更され得る行を初期状態に戻す。

TRUNCATE TABLE sessions;

SET @m = (SELECT MAX(id) FROM isuauction_init.users);
DELETE FROM users WHERE id > @m;

SET @m = (SELECT MAX(id) FROM isuauction_init.images);
DELETE FROM images WHERE id > @m;

SET @m = (SELECT MAX(id) FROM isuauction_init.items);
DELETE FROM items WHERE id > @m;

SET @m = (SELECT MAX(id) FROM isuauction_init.bids);
DELETE FROM bids WHERE id > @m;

SET @m = (SELECT MAX(id) FROM isuauction_init.notifications);
DELETE FROM notifications WHERE id > @m;

-- 出品中の初期商品（終了は遠い未来）は入札で状態が変わり得るので戻す
UPDATE items i JOIN isuauction_init.items o ON o.id = i.id
SET i.status = o.status, i.end_at = o.end_at, i.winner_id = o.winner_id, i.final_price = o.final_price
WHERE o.status = 'open';

TRUNCATE TABLE watches;
INSERT INTO watches SELECT * FROM isuauction_init.watches;
