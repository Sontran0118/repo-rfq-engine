import { Injectable, NgZone, signal } from '@angular/core';
import { HttpClient } from '@angular/common/http';
import { Observable } from 'rxjs';

import { Counterparty, Direction, RFQ, Trade } from './models';

const API = '/api';

export interface CreateRFQRequest {
  clientId: string;
  direction: Direction;
  notional: number;
  currency: string;
  termDays: number;
  ttlSeconds: number;
  collateral: { cusip: string; description: string; assetClass: string };
}

/**
 * Talks to the Go service and keeps a live view of the book.
 *
 * Reads go over REST; changes arrive on a server-sent-event stream. The stream
 * carries only a signal that something moved — the service then re-reads the
 * affected list. Pushing full state through the stream would mean two code paths
 * that can disagree about what the book looks like.
 */
@Injectable({ providedIn: 'root' })
export class RfqService {
  readonly rfqs = signal<RFQ[]>([]);
  readonly trades = signal<Trade[]>([]);
  readonly counterparties = signal<Counterparty[]>([]);
  readonly connected = signal(false);
  readonly lastError = signal<string | null>(null);

  private stream?: EventSource;

  constructor(
    private http: HttpClient,
    private zone: NgZone,
  ) {}

  /** Loads the book and opens the live stream. */
  start(): void {
    this.refreshAll();
    this.connect();
  }

  stop(): void {
    this.stream?.close();
    this.stream = undefined;
    this.connected.set(false);
  }

  refreshAll(): void {
    this.loadRFQs();
    this.loadTrades();
    this.loadCounterparties();
  }

  loadRFQs(): void {
    this.http.get<RFQ[]>(`${API}/rfqs`).subscribe({
      next: (rfqs) => this.rfqs.set(rfqs ?? []),
      error: (e) => this.lastError.set(this.describe(e)),
    });
  }

  loadTrades(): void {
    this.http.get<Trade[]>(`${API}/trades`).subscribe({
      next: (trades) => this.trades.set(trades ?? []),
      error: (e) => this.lastError.set(this.describe(e)),
    });
  }

  loadCounterparties(): void {
    this.http.get<Counterparty[]>(`${API}/counterparties`).subscribe({
      next: (cps) => this.counterparties.set(cps ?? []),
      error: (e) => this.lastError.set(this.describe(e)),
    });
  }

  createRFQ(req: CreateRFQRequest): Observable<RFQ> {
    return this.http.post<RFQ>(`${API}/rfqs`, req);
  }

  submitQuote(
    rfqId: string,
    dealerId: string,
    rate: number,
    haircut: number,
  ): Observable<unknown> {
    return this.http.post(`${API}/rfqs/${rfqId}/quotes`, { dealerId, rate, haircut });
  }

  /** Accepts a specific quote, or the best price when quoteId is omitted. */
  acceptQuote(rfqId: string, quoteId = ''): Observable<Trade> {
    return this.http.post<Trade>(`${API}/rfqs/${rfqId}/accept`, { quoteId });
  }

  cancelRFQ(rfqId: string): Observable<unknown> {
    return this.http.post(`${API}/rfqs/${rfqId}/cancel`, {});
  }

  /**
   * Turns an HTTP failure into something a trader can act on.
   *
   * 409 is the one that matters: it means the book moved under you, not that
   * the request was malformed or that retrying would help.
   */
  describe(err: { status?: number; error?: { error?: string } }): string {
    const detail = err?.error?.error;
    switch (err?.status) {
      case 0:
        return 'Cannot reach the service — is it running on :8080?';
      case 409:
        return detail ?? 'The book moved — this RFQ is no longer live.';
      case 404:
        return detail ?? 'Not found.';
      case 400:
        return detail ?? 'Invalid request.';
      default:
        return detail ?? 'Unexpected error.';
    }
  }

  private connect(): void {
    this.stream?.close();
    const es = new EventSource(`${API}/events`);
    this.stream = es;

    es.onopen = () => this.zone.run(() => this.connected.set(true));

    es.onerror = () => {
      // EventSource reconnects on its own; surface the gap without tearing down.
      this.zone.run(() => this.connected.set(false));
    };

    const reloadRFQs = () => this.zone.run(() => this.loadRFQs());
    for (const type of ['rfq.created', 'quote.submitted', 'rfq.cancelled', 'rfq.expired']) {
      es.addEventListener(type, reloadRFQs);
    }

    // A trade changes both sides of the screen.
    es.addEventListener('trade.executed', () =>
      this.zone.run(() => {
        this.loadRFQs();
        this.loadTrades();
      }),
    );
  }
}
