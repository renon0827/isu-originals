SET NAMES utf8mb4;

DROP TABLE IF EXISTS `users`;
CREATE TABLE `users` (
  `id` BIGINT NOT NULL AUTO_INCREMENT,
  `login_name` VARCHAR(64) NOT NULL,
  `nickname` VARCHAR(128) NOT NULL,
  `password_hash` CHAR(64) NOT NULL,
  `salt` CHAR(16) NOT NULL,
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

DROP TABLE IF EXISTS `categories`;
CREATE TABLE `categories` (
  `id` BIGINT NOT NULL AUTO_INCREMENT,
  `name` VARCHAR(64) NOT NULL,
  PRIMARY KEY (`id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

DROP TABLE IF EXISTS `images`;
CREATE TABLE `images` (
  `id` BIGINT NOT NULL AUTO_INCREMENT,
  `data` MEDIUMBLOB NOT NULL,
  PRIMARY KEY (`id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

DROP TABLE IF EXISTS `items`;
CREATE TABLE `items` (
  `id` BIGINT NOT NULL AUTO_INCREMENT,
  `seller_id` BIGINT NOT NULL,
  `category_id` BIGINT NOT NULL,
  `title` VARCHAR(255) NOT NULL,
  `description` TEXT NOT NULL,
  `image_id` BIGINT NOT NULL,
  `start_price` INT NOT NULL,
  `increment` INT NOT NULL,
  -- open / ended
  `status` VARCHAR(16) NOT NULL,
  `end_at` DATETIME(6) NOT NULL,
  -- 出品時の終了予定時刻（終了間際の入札で end_at は延長される）
  `original_end_at` DATETIME(6) NOT NULL,
  `winner_id` BIGINT NULL,
  `final_price` INT NULL,
  `created_at` DATETIME(6) NOT NULL,
  PRIMARY KEY (`id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

DROP TABLE IF EXISTS `bids`;
CREATE TABLE `bids` (
  `id` BIGINT NOT NULL AUTO_INCREMENT,
  `item_id` BIGINT NOT NULL,
  `user_id` BIGINT NOT NULL,
  -- 自動入札の上限額（この金額まで自動で競り上げる）
  `max_amount` INT NOT NULL,
  `created_at` DATETIME(6) NOT NULL,
  PRIMARY KEY (`id`),
  KEY `idx_item_id` (`item_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

DROP TABLE IF EXISTS `watches`;
CREATE TABLE `watches` (
  `user_id` BIGINT NOT NULL,
  `item_id` BIGINT NOT NULL,
  `created_at` DATETIME(6) NOT NULL,
  PRIMARY KEY (`user_id`, `item_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

DROP TABLE IF EXISTS `notifications`;
CREATE TABLE `notifications` (
  `id` BIGINT NOT NULL AUTO_INCREMENT,
  `user_id` BIGINT NOT NULL,
  -- outbid / won / sold / ended
  `type` VARCHAR(16) NOT NULL,
  `item_id` BIGINT NOT NULL,
  `price` INT NULL,
  `created_at` DATETIME(6) NOT NULL,
  PRIMARY KEY (`id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
