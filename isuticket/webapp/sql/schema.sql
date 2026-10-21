SET NAMES utf8mb4;

DROP TABLE IF EXISTS `users`;
CREATE TABLE `users` (
  `id` BIGINT NOT NULL AUTO_INCREMENT,
  `login_name` VARCHAR(64) NOT NULL,
  `display_name` VARCHAR(128) NOT NULL,
  `password_hash` CHAR(64) NOT NULL,
  `salt` CHAR(16) NOT NULL,
  `is_organizer` TINYINT(1) NOT NULL DEFAULT 0,
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

DROP TABLE IF EXISTS `venues`;
CREATE TABLE `venues` (
  `id` BIGINT NOT NULL AUTO_INCREMENT,
  `name` VARCHAR(128) NOT NULL,
  -- [{"name":"アリーナA","rank":"S","rows":20,"cols":30}, ...]
  `layout` JSON NOT NULL,
  PRIMARY KEY (`id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

DROP TABLE IF EXISTS `events`;
CREATE TABLE `events` (
  `id` BIGINT NOT NULL AUTO_INCREMENT,
  `venue_id` BIGINT NOT NULL,
  `title` VARCHAR(255) NOT NULL,
  `artist` VARCHAR(128) NOT NULL,
  `description` TEXT NOT NULL,
  -- first_come: 先着 / lottery: 抽選
  `sale_type` VARCHAR(16) NOT NULL,
  `sale_start_at` DATETIME(6) NOT NULL,
  `sale_end_at` DATETIME(6) NOT NULL,
  -- 抽選結果の発表時刻（先着は NULL）
  `announce_at` DATETIME(6) NULL,
  `lottery_drawn` TINYINT(1) NOT NULL DEFAULT 0,
  `gate_open_at` DATETIME(6) NOT NULL,
  `end_at` DATETIME(6) NOT NULL,
  `image` MEDIUMBLOB NOT NULL,
  `created_at` DATETIME(6) NOT NULL,
  PRIMARY KEY (`id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

DROP TABLE IF EXISTS `event_ranks`;
CREATE TABLE `event_ranks` (
  `event_id` BIGINT NOT NULL,
  `rank` CHAR(1) NOT NULL,
  `price` INT NOT NULL,
  PRIMARY KEY (`event_id`, `rank`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

DROP TABLE IF EXISTS `event_seats`;
CREATE TABLE `event_seats` (
  `id` BIGINT NOT NULL AUTO_INCREMENT,
  `event_id` BIGINT NOT NULL,
  `section` VARCHAR(64) NOT NULL,
  `row_no` INT NOT NULL,
  `col_no` INT NOT NULL,
  `rank` CHAR(1) NOT NULL,
  -- available / sold
  `status` VARCHAR(16) NOT NULL,
  `ticket_id` BIGINT NULL,
  `updated_at` DATETIME(6) NOT NULL,
  PRIMARY KEY (`id`),
  KEY `idx_event_id` (`event_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

DROP TABLE IF EXISTS `orders`;
CREATE TABLE `orders` (
  `id` BIGINT NOT NULL AUTO_INCREMENT,
  `user_id` BIGINT NOT NULL,
  `event_id` BIGINT NOT NULL,
  -- first_come / lottery / resale
  `kind` VARCHAR(16) NOT NULL,
  `total` INT NOT NULL,
  `payment_id` VARCHAR(64) NULL,
  `created_at` DATETIME(6) NOT NULL,
  PRIMARY KEY (`id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

DROP TABLE IF EXISTS `tickets`;
CREATE TABLE `tickets` (
  `id` BIGINT NOT NULL AUTO_INCREMENT,
  `order_id` BIGINT NOT NULL,
  `event_id` BIGINT NOT NULL,
  `event_seat_id` BIGINT NOT NULL,
  `user_id` BIGINT NOT NULL,
  -- 所有者が変わるたびに振り直す。QR トークンの payload に含まれる
  `serial` CHAR(32) NOT NULL,
  -- valid / listed（リセール出品中）
  `status` VARCHAR(16) NOT NULL,
  `used_at` DATETIME(6) NULL,
  `created_at` DATETIME(6) NOT NULL,
  PRIMARY KEY (`id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

DROP TABLE IF EXISTS `lottery_entries`;
CREATE TABLE `lottery_entries` (
  `id` BIGINT NOT NULL AUTO_INCREMENT,
  `event_id` BIGINT NOT NULL,
  `user_id` BIGINT NOT NULL,
  `rank` CHAR(1) NOT NULL,
  `quantity` INT NOT NULL,
  -- pending / won / lost
  `status` VARCHAR(16) NOT NULL,
  `created_at` DATETIME(6) NOT NULL,
  PRIMARY KEY (`id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

DROP TABLE IF EXISTS `resale_listings`;
CREATE TABLE `resale_listings` (
  `id` BIGINT NOT NULL AUTO_INCREMENT,
  `ticket_id` BIGINT NOT NULL,
  `event_id` BIGINT NOT NULL,
  `seller_id` BIGINT NOT NULL,
  `price` INT NOT NULL,
  -- listed / sold / cancelled
  `status` VARCHAR(16) NOT NULL,
  `buyer_id` BIGINT NULL,
  `created_at` DATETIME(6) NOT NULL,
  `sold_at` DATETIME(6) NULL,
  PRIMARY KEY (`id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

DROP TABLE IF EXISTS `settings`;
CREATE TABLE `settings` (
  `name` VARCHAR(64) NOT NULL,
  `value` VARCHAR(255) NOT NULL,
  PRIMARY KEY (`name`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
