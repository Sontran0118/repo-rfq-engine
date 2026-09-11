import { TestBed } from '@angular/core/testing';
import { HttpTestingController, provideHttpClientTesting } from '@angular/common/http/testing';
import { provideHttpClient } from '@angular/common/http';

import { RfqService } from './rfq.service';
import { RFQ } from './models';

describe('RfqService', () => {
  let svc: RfqService;
  let http: HttpTestingController;

  beforeEach(() => {
    TestBed.configureTestingModule({
      providers: [RfqService, provideHttpClient(), provideHttpClientTesting()],
    });
    svc = TestBed.inject(RfqService);
    http = TestBed.inject(HttpTestingController);
  });

  afterEach(() => http.verify());

  it('loads RFQs into the signal', () => {
    svc.loadRFQs();

    const req = http.expectOne('/api/rfqs');
    expect(req.request.method).toBe('GET');
    req.flush([{ id: 'rfq-1', state: 'OPEN' } as RFQ]);

    expect(svc.rfqs().length).toBe(1);
    expect(svc.rfqs()[0].id).toBe('rfq-1');
  });

  it('tolerates a null list rather than crashing the blotter', () => {
    svc.loadRFQs();
    http.expectOne('/api/rfqs').flush(null);
    expect(svc.rfqs()).toEqual([]);
  });

  it('posts an RFQ with notional in minor units', () => {
    svc
      .createRFQ({
        clientId: 'client-a',
        direction: 'REPO',
        notional: 1_000_000_00,
        currency: 'USD',
        termDays: 1,
        ttlSeconds: 300,
        collateral: { cusip: '912810TM0', description: 'UST', assetClass: 'UST' },
      })
      .subscribe();

    const req = http.expectOne('/api/rfqs');
    expect(req.request.method).toBe('POST');
    expect(req.request.body.notional).toBe(100_000_000);
    req.flush({});
  });

  it('sends an empty quoteId to accept the best price', () => {
    svc.acceptQuote('rfq-1').subscribe();

    const req = http.expectOne('/api/rfqs/rfq-1/accept');
    expect(req.request.body).toEqual({ quoteId: '' });
    req.flush({});
  });

  it('surfaces a server error message on load failure', () => {
    svc.loadRFQs();
    http.expectOne('/api/rfqs').flush(
      { error: 'database is down' },
      { status: 500, statusText: 'Server Error' },
    );
    expect(svc.lastError()).toBe('database is down');
  });

  /**
   * The 409 path is the one that matters operationally: it means another trader
   * won the RFQ, so the UI must say "the book moved" rather than inviting a
   * retry that can never succeed.
   */
  describe('describe()', () => {
    it('explains a conflict as the book having moved', () => {
      expect(svc.describe({ status: 409, error: {} })).toContain('book moved');
    });

    it('prefers the server detail when present', () => {
      expect(
        svc.describe({ status: 409, error: { error: 'rfq has already been executed' } }),
      ).toBe('rfq has already been executed');
    });

    it('explains an unreachable service', () => {
      expect(svc.describe({ status: 0 })).toContain('Cannot reach the service');
    });

    it('handles 404 and 400', () => {
      expect(svc.describe({ status: 404, error: {} })).toBe('Not found.');
      expect(svc.describe({ status: 400, error: {} })).toBe('Invalid request.');
    });

    it('falls back for an unrecognised status', () => {
      expect(svc.describe({ status: 503, error: {} })).toBe('Unexpected error.');
    });
  });
});
