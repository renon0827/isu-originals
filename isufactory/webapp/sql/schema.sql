SET NAMES utf8mb4;

DROP TABLE IF EXISTS `users`;
CREATE TABLE `users` (
  `id` BIGINT NOT NULL AUTO_INCREMENT,
  `login_name` VARCHAR(64) NOT NULL,
  `display_name` VARCHAR(128) NOT NULL,
  `password_hash` CHAR(64) NOT NULL,
  `salt` CHAR(16) NOT NULL,
  -- operator（現場担当）/ planner（生産計画担当）
  `role` VARCHAR(16) NOT NULL,
  `created_at` DATETIME(6) NOT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uniq_login_name` (`login_name`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

DROP TABLE IF EXISTS `sessions`;
CREATE TABLE `sessions` (
  `id` CHAR(64) NOT NULL,
  `user_id` BIGINT NOT NULL,
  `created_at` DATETIME(6) NOT NULL,
  PRIMARY KEY (`id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

DROP TABLE IF EXISTS `lines`;
CREATE TABLE `lines` (
  `id` BIGINT NOT NULL AUTO_INCREMENT,
  `name` VARCHAR(64) NOT NULL,
  PRIMARY KEY (`id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

DROP TABLE IF EXISTS `machines`;
CREATE TABLE `machines` (
  `id` BIGINT NOT NULL AUTO_INCREMENT,
  `line_id` BIGINT NOT NULL,
  `name` VARCHAR(64) NOT NULL,
  -- press / weld / paint / assembly
  `machine_type` VARCHAR(16) NOT NULL,
  -- センサー送信端末の認証トークン
  `token` CHAR(32) NOT NULL,
  PRIMARY KEY (`id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

DROP TABLE IF EXISTS `products`;
CREATE TABLE `products` (
  `id` BIGINT NOT NULL AUTO_INCREMENT,
  `name` VARCHAR(128) NOT NULL,
  -- 製品ファミリー（A〜F）。同じ設備で違うファミリーに切り替えると段取り替えが必要
  `family` CHAR(1) NOT NULL,
  `machine_type` VARCHAR(16) NOT NULL,
  -- 1 個あたりの加工時間（秒）
  `unit_seconds` INT NOT NULL,
  -- 図面（PNG）
  `drawing` MEDIUMBLOB NOT NULL,
  PRIMARY KEY (`id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

DROP TABLE IF EXISTS `materials`;
CREATE TABLE `materials` (
  `id` BIGINT NOT NULL AUTO_INCREMENT,
  `name` VARCHAR(128) NOT NULL,
  -- 引当可能な在庫数
  `stock` BIGINT NOT NULL,
  PRIMARY KEY (`id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

DROP TABLE IF EXISTS `bom`;
CREATE TABLE `bom` (
  `product_id` BIGINT NOT NULL,
  `material_id` BIGINT NOT NULL,
  -- 製品 1 個あたりの必要数
  `quantity` INT NOT NULL,
  PRIMARY KEY (`product_id`, `material_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

DROP TABLE IF EXISTS `material_receipts`;
CREATE TABLE `material_receipts` (
  `id` BIGINT NOT NULL AUTO_INCREMENT,
  `material_id` BIGINT NOT NULL,
  `quantity` INT NOT NULL,
  `created_at` DATETIME(6) NOT NULL,
  PRIMARY KEY (`id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

DROP TABLE IF EXISTS `orders`;
CREATE TABLE `orders` (
  `id` BIGINT NOT NULL AUTO_INCREMENT,
  `product_id` BIGINT NOT NULL,
  `quantity` INT NOT NULL,
  `due_at` DATETIME(6) NOT NULL,
  -- pending（未計画）/ planned（計画済み）
  `status` VARCHAR(16) NOT NULL,
  `created_at` DATETIME(6) NOT NULL,
  PRIMARY KEY (`id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

DROP TABLE IF EXISTS `assignments`;
CREATE TABLE `assignments` (
  `id` BIGINT NOT NULL AUTO_INCREMENT,
  `order_id` BIGINT NOT NULL,
  `machine_id` BIGINT NOT NULL,
  `family` CHAR(1) NOT NULL,
  -- 段取り替えを含む作業の開始・終了
  `start_at` DATETIME(6) NOT NULL,
  `end_at` DATETIME(6) NOT NULL,
  `created_at` DATETIME(6) NOT NULL,
  PRIMARY KEY (`id`),
  KEY `idx_machine_id` (`machine_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

DROP TABLE IF EXISTS `readings`;
CREATE TABLE `readings` (
  `id` BIGINT NOT NULL AUTO_INCREMENT,
  `machine_id` BIGINT NOT NULL,
  -- temperature / vibration / current
  `metric` VARCHAR(16) NOT NULL,
  `value` DOUBLE NOT NULL,
  `measured_at` DATETIME(6) NOT NULL,
  PRIMARY KEY (`id`),
  KEY `idx_machine_metric` (`machine_id`, `metric`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

DROP TABLE IF EXISTS `alerts`;
CREATE TABLE `alerts` (
  `id` BIGINT NOT NULL AUTO_INCREMENT,
  `machine_id` BIGINT NOT NULL,
  `metric` VARCHAR(16) NOT NULL,
  `value` DOUBLE NOT NULL,
  -- 判定に使った直近 10 分の平均
  `baseline` DOUBLE NOT NULL,
  `measured_at` DATETIME(6) NOT NULL,
  `created_at` DATETIME(6) NOT NULL,
  PRIMARY KEY (`id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

DROP TABLE IF EXISTS `settings`;
CREATE TABLE `settings` (
  `name` VARCHAR(64) NOT NULL,
  `value` VARCHAR(255) NOT NULL,
  PRIMARY KEY (`name`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
