import { Quote, RFQ, bestQuote, formatBps, formatMoney } from './models';

/**
 * These mirror the Go tests in internal/domain/rfq_test.go on purpose.
 *
 * The blotter highlights the winning quote client-side so the UI does not lag
 * the quote feed. If the two implementations disagree, the screen shows one
 * dealer as best while the server trades with another — so the rule is pinned
 * on both sides with the same cases.
 */

function quote(over: Partial<Quote> = {}): Quote {
  return {
    id: 'q1',
    rfqId: 'rfq-1',
    dealerId: 'dealer-a',
    rate: 525,
    haircut: 200,
    status: 'ACTIVE',
    createdAt: '2026-09-11T10:00:00Z',
    ...over,
  };
}

function rfq(direction: 'REPO' | 'REVERSE_REPO', quotes: Quote[]): RFQ {
  return {
    id: 'rfq-1',
    clientId: 'client-a',
    direction,
    collateral: { cusip: '912810TM0', description: 'UST', assetClass: 'UST' },
    notional: 1_000_000_00,
    currency: 'USD',
    startDate: '2026-09-11T00:00:00Z',
    endDate: '2026-09-12T00:00:00Z',
    state: 'QUOTED',
    expiresAt: '2026-09-11T10:05:00Z',
    createdAt: '2026-09-11T10:00:00Z',
    quotes,
  };
}

describe('bestQuote', () => {
  const spread = [
    quote({ id: 'q1', dealerId: 'dealer-a', rate: 530, createdAt: '2026-09-11T10:00:00Z' }),
    quote({ id: 'q2', dealerId: 'dealer-b', rate: 515, createdAt: '2026-09-11T10:00:01Z' }),
    quote({ id: 'q3', dealerId: 'dealer-c', rate: 545, createdAt: '2026-09-11T10:00:02Z' }),
  ];

  it('picks the lowest rate for a repo, where the client pays', () => {
    expect(bestQuote(rfq('REPO', spread))?.id).toBe('q2');
  });

  it('picks the highest rate for a reverse repo, where the client earns', () => {
    expect(bestQuote(rfq('REVERSE_REPO', spread))?.id).toBe('q3');
  });

  it('breaks equal rates on the lower haircut', () => {
    const tied = [
      quote({ id: 'q1', rate: 525, haircut: 200 }),
      quote({ id: 'q2', rate: 525, haircut: 50 }),
    ];
    expect(bestQuote(rfq('REPO', tied))?.id).toBe('q2');
  });

  it('breaks identical quotes on who priced first', () => {
    const tied = [
      quote({ id: 'q_late', createdAt: '2026-09-11T10:00:01Z' }),
      quote({ id: 'q_first', createdAt: '2026-09-11T10:00:00Z' }),
    ];
    expect(bestQuote(rfq('REPO', tied))?.id).toBe('q_first');
  });

  it('ignores withdrawn and lost quotes even when they price better', () => {
    const mixed = [
      quote({ id: 'q_withdrawn', rate: 400, status: 'WITHDRAWN' }),
      quote({ id: 'q_lost', rate: 410, status: 'LOST' }),
      quote({ id: 'q_active', rate: 525, status: 'ACTIVE' }),
    ];
    expect(bestQuote(rfq('REPO', mixed))?.id).toBe('q_active');
  });

  it('returns null when there are no quotes', () => {
    expect(bestQuote(rfq('REPO', []))).toBeNull();
  });

  it('returns null when an RFQ has no quotes field at all', () => {
    const bare = rfq('REPO', []);
    delete bare.quotes;
    expect(bestQuote(bare)).toBeNull();
  });
});

describe('formatMoney', () => {
  it('renders minor units as dollars', () => {
    expect(formatMoney(1_000_000_00)).toBe('$1,000,000.00');
  });

  it('keeps cents exact', () => {
    expect(formatMoney(14_306)).toBe('$143.06');
  });

  it('handles zero', () => {
    expect(formatMoney(0)).toBe('$0.00');
  });
});

describe('formatBps', () => {
  it('renders basis points as a percentage', () => {
    expect(formatBps(525)).toBe('5.25%');
  });

  it('preserves the sign on special collateral', () => {
    expect(formatBps(-25)).toBe('-0.25%');
  });

  it('renders a zero rate', () => {
    expect(formatBps(0)).toBe('0.00%');
  });
});
