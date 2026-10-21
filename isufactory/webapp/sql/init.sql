-- ベンチマーク走行前の初期化。
-- 初期データは isufactory_init データベースに同じ内容が入っている（プロビジョニング時に作成）。
-- 走行中に追加された行を消し、走行中に変更され得る行（部材在庫）だけを初期状態に戻す。

TRUNCATE TABLE sessions;
DELETE FROM settings;
INSERT INTO settings (name, value) VALUES ('plan_lock', '');

SET @m = (SELECT MAX(id) FROM isufactory_init.orders);
DELETE FROM orders WHERE id > @m;

SET @m = (SELECT MAX(id) FROM isufactory_init.assignments);
DELETE FROM assignments WHERE id > @m;

SET @m = (SELECT MAX(id) FROM isufactory_init.readings);
DELETE FROM readings WHERE id > @m;

SET @m = (SELECT MAX(id) FROM isufactory_init.alerts);
DELETE FROM alerts WHERE id > @m;

SET @m = (SELECT MAX(id) FROM isufactory_init.material_receipts);
DELETE FROM material_receipts WHERE id > @m;

UPDATE materials m JOIN isufactory_init.materials i ON i.id = m.id SET m.stock = i.stock;
