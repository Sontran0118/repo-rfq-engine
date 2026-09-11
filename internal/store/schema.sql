-- repo-rfq-engine schema (MySQL 8.0+, InnoDB)
--
-- Design notes:
--   * Cash is BIGINT minor units, never DECIMAL-as-float. Rates and haircuts are
--     signed integer basis points — negative rates are legal on special collateral.
--   * The rfq.state column is the concurrency control point. Accepting a quote
--     takes a row lock on it (SELECT ... FOR UPDATE), which is what makes
--     "exactly one dealer wins" hold under concurrent acceptance.
--   * trade.rfq_id is UNIQUE. Even if the lock were somehow bypassed, the
--     database still refuses to book a second trade against one request.

CREATE TABLE IF NOT EXISTS counterparties (
    id         VARCHAR(36)  NOT NULL PRIMARY KEY,
    name       VARCHAR(128) NOT NULL,
    kind       ENUM('CLIENT', 'DEALER') NOT NULL,
    created_at DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3)
) ENGINE = InnoDB;

CREATE TABLE IF NOT EXISTS rfqs (
    id                   VARCHAR(36) NOT NULL PRIMARY KEY,
    client_id            VARCHAR(36) NOT NULL,
    direction            ENUM('REPO', 'REVERSE_REPO') NOT NULL,
    collateral_cusip     VARCHAR(12)  NOT NULL,
    collateral_desc      VARCHAR(255) NOT NULL DEFAULT '',
    collateral_asset_cls VARCHAR(16)  NOT NULL DEFAULT '',
    notional             BIGINT      NOT NULL,
    currency             CHAR(3)     NOT NULL,
    start_date           DATE        NOT NULL,
    end_date             DATE        NOT NULL,
    state                ENUM('OPEN', 'QUOTED', 'EXECUTED', 'CANCELLED', 'EXPIRED')
                             NOT NULL DEFAULT 'OPEN',
    expires_at           DATETIME(3) NOT NULL,
    created_at           DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),

    CONSTRAINT fk_rfq_client FOREIGN KEY (client_id) REFERENCES counterparties (id),
    CONSTRAINT ck_rfq_notional CHECK (notional > 0),
    CONSTRAINT ck_rfq_tenor CHECK (end_date > start_date),

    -- The blotter's default view is "live RFQs, newest first".
    INDEX idx_rfq_state_created (state, created_at DESC),
    INDEX idx_rfq_client (client_id)
) ENGINE = InnoDB;

CREATE TABLE IF NOT EXISTS quotes (
    id         VARCHAR(36) NOT NULL PRIMARY KEY,
    rfq_id     VARCHAR(36) NOT NULL,
    dealer_id  VARCHAR(36) NOT NULL,
    rate_bps   INT         NOT NULL,
    haircut_bps INT        NOT NULL,
    status     ENUM('ACTIVE', 'WITHDRAWN', 'WON', 'LOST') NOT NULL DEFAULT 'ACTIVE',
    created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),

    CONSTRAINT fk_quote_rfq FOREIGN KEY (rfq_id) REFERENCES rfqs (id) ON DELETE CASCADE,
    CONSTRAINT fk_quote_dealer FOREIGN KEY (dealer_id) REFERENCES counterparties (id),
    CONSTRAINT ck_quote_haircut CHECK (haircut_bps >= 0),

    -- A dealer prices an RFQ once; re-pricing updates the existing quote rather
    -- than stacking duplicates that would distort best-quote selection.
    UNIQUE KEY uq_quote_rfq_dealer (rfq_id, dealer_id),
    INDEX idx_quote_rfq (rfq_id, status)
) ENGINE = InnoDB;

CREATE TABLE IF NOT EXISTS trades (
    id                  VARCHAR(36) NOT NULL PRIMARY KEY,
    rfq_id              VARCHAR(36) NOT NULL,
    quote_id            VARCHAR(36) NOT NULL,
    client_id           VARCHAR(36) NOT NULL,
    dealer_id           VARCHAR(36) NOT NULL,
    direction           ENUM('REPO', 'REVERSE_REPO') NOT NULL,
    collateral_cusip    VARCHAR(12) NOT NULL,
    notional            BIGINT      NOT NULL,
    currency            CHAR(3)     NOT NULL,
    rate_bps            INT         NOT NULL,
    haircut_bps         INT         NOT NULL,
    start_date          DATE        NOT NULL,
    end_date            DATE        NOT NULL,
    term_days           INT         NOT NULL,
    interest            BIGINT      NOT NULL,
    repurchase_price    BIGINT      NOT NULL,
    collateral_required BIGINT      NOT NULL,
    executed_at         DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),

    CONSTRAINT fk_trade_rfq FOREIGN KEY (rfq_id) REFERENCES rfqs (id),
    CONSTRAINT fk_trade_quote FOREIGN KEY (quote_id) REFERENCES quotes (id),

    -- One request, at most one trade. The backstop behind the row lock.
    UNIQUE KEY uq_trade_rfq (rfq_id),
    INDEX idx_trade_executed (executed_at DESC)
) ENGINE = InnoDB;
