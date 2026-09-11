/** Wire types mirroring the Go domain package. */

export type Direction = 'REPO' | 'REVERSE_REPO';

export type RFQState = 'OPEN' | 'QUOTED' | 'EXECUTED' | 'CANCELLED' | 'EXPIRED';

export type QuoteStatus = 'ACTIVE' | 'WITHDRAWN' | 'WON' | 'LOST';

export interface Collateral {
  cusip: string;
  description: string;
  assetClass: string;
}

export interface Quote {
  id: string;
  rfqId: string;
  dealerId: string;
  /** Repo rate in basis points. Negative is legal on special collateral. */
  rate: number;
  /** Collateral margin in basis points. */
  haircut: number;
  status: QuoteStatus;
  createdAt: string;
}

export interface RFQ {
  id: string;
  clientId: string;
  direction: Direction;
  collateral: Collateral;
  /** Cash amount in minor units (cents). */
  notional: number;
  currency: string;
  startDate: string;
  endDate: string;
  state: RFQState;
  expiresAt: string;
  createdAt: string;
  quotes?: Quote[];
}

export interface Trade {
  id: string;
  rfqId: string;
  quoteId: string;
  clientId: string;
  dealerId: string;
  direction: Direction;
  collateral: Collateral;
  notional: number;
  currency: string;
  rate: number;
  haircut: number;
  startDate: string;
  endDate: string;
  termDays: number;
  interest: number;
  repurchasePrice: number;
  collateralRequired: number;
  executedAt: string;
}

export interface Counterparty {
  id: string;
  name: string;
  kind: 'CLIENT' | 'DEALER';
}

/**
 * Picks the quote the client should accept.
 *
 * This mirrors `RFQ.BestQuote` in the Go domain deliberately: the blotter has to
 * highlight the winning price before the user clicks, and round-tripping to the
 * server for that would make the highlight lag the quote feed. The server stays
 * authoritative — it recomputes this inside the accept transaction, so a stale
 * client cannot cause the wrong quote to trade.
 */
export function bestQuote(rfq: RFQ): Quote | null {
  const active = (rfq.quotes ?? []).filter((q) => q.status === 'ACTIVE');
  if (active.length === 0) return null;

  return active.reduce((best, q) => {
    if (q.rate !== best.rate) {
      // A repo client pays the rate and wants it low; a reverse-repo client
      // earns it and wants it high.
      return rfq.direction === 'REPO'
        ? q.rate < best.rate
          ? q
          : best
        : q.rate > best.rate
          ? q
          : best;
    }
    if (q.haircut !== best.haircut) return q.haircut < best.haircut ? q : best;
    return q.createdAt < best.createdAt ? q : best;
  });
}

/** Formats minor units as a currency amount with thousands separators. */
export function formatMoney(minorUnits: number, currency = 'USD'): string {
  return new Intl.NumberFormat('en-US', {
    style: 'currency',
    currency,
    minimumFractionDigits: 2,
  }).format(minorUnits / 100);
}

/** Formats basis points as a percentage, preserving the sign. */
export function formatBps(bps: number): string {
  return `${(bps / 100).toFixed(2)}%`;
}
