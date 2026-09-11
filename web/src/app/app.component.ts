import { Component, OnDestroy, OnInit, computed, signal } from '@angular/core';
import { CommonModule } from '@angular/common';
import { FormsModule } from '@angular/forms';

import { RfqService } from './rfq.service';
import { RFQ, Trade, bestQuote, formatBps, formatMoney } from './models';

@Component({
  selector: 'app-root',
  standalone: true,
  imports: [CommonModule, FormsModule],
  templateUrl: './app.component.html',
  styleUrl: './app.component.css',
})
export class AppComponent implements OnInit, OnDestroy {
  readonly bestQuote = bestQuote;
  readonly formatMoney = formatMoney;
  readonly formatBps = formatBps;

  readonly toast = signal<{ text: string; kind: 'ok' | 'err' } | null>(null);
  readonly selected = signal<RFQ | null>(null);

  // New-RFQ ticket. Notional is entered in whole dollars and converted to the
  // minor units the API expects on submit.
  ticket = {
    clientId: '',
    direction: 'REPO' as 'REPO' | 'REVERSE_REPO',
    notionalDollars: 10_000_000,
    cusip: '912810TM0',
    description: 'US TREASURY N/B 4.25% 2054',
    assetClass: 'UST',
    termDays: 1,
    ttlSeconds: 300,
  };

  // Dealer quote panel.
  quoteForm = { dealerId: '', ratePct: 5.25, haircutPct: 2.0 };

  readonly clients = computed(() =>
    this.svc.counterparties().filter((c) => c.kind === 'CLIENT'),
  );
  readonly dealers = computed(() =>
    this.svc.counterparties().filter((c) => c.kind === 'DEALER'),
  );
  readonly liveRFQs = computed(() =>
    this.svc.rfqs().filter((r) => r.state === 'OPEN' || r.state === 'QUOTED'),
  );

  constructor(public svc: RfqService) {}

  ngOnInit(): void {
    this.svc.start();

    // Default the pickers to the first counterparty of each kind once loaded.
    const seed = setInterval(() => {
      if (this.clients().length && !this.ticket.clientId) {
        this.ticket.clientId = this.clients()[0].id;
      }
      if (this.dealers().length && !this.quoteForm.dealerId) {
        this.quoteForm.dealerId = this.dealers()[0].id;
      }
      if (this.ticket.clientId && this.quoteForm.dealerId) clearInterval(seed);
    }, 200);
  }

  ngOnDestroy(): void {
    this.svc.stop();
  }

  createRFQ(): void {
    this.svc
      .createRFQ({
        clientId: this.ticket.clientId,
        direction: this.ticket.direction,
        notional: Math.round(this.ticket.notionalDollars * 100),
        currency: 'USD',
        termDays: this.ticket.termDays,
        ttlSeconds: this.ticket.ttlSeconds,
        collateral: {
          cusip: this.ticket.cusip,
          description: this.ticket.description,
          assetClass: this.ticket.assetClass,
        },
      })
      .subscribe({
        next: (rfq) => this.notify(`RFQ raised for ${formatMoney(rfq.notional)}`, 'ok'),
        error: (e) => this.notify(this.svc.describe(e), 'err'),
      });
  }

  submitQuote(rfq: RFQ): void {
    this.svc
      .submitQuote(
        rfq.id,
        this.quoteForm.dealerId,
        Math.round(this.quoteForm.ratePct * 100),
        Math.round(this.quoteForm.haircutPct * 100),
      )
      .subscribe({
        next: () =>
          this.notify(
            `${this.quoteForm.dealerId} quoted ${this.quoteForm.ratePct.toFixed(2)}%`,
            'ok',
          ),
        error: (e) => this.notify(this.svc.describe(e), 'err'),
      });
  }

  accept(rfq: RFQ): void {
    this.svc.acceptQuote(rfq.id).subscribe({
      next: (t: Trade) =>
        this.notify(
          `Traded with ${t.dealerId} at ${formatBps(t.rate)} — interest ${formatMoney(t.interest)}`,
          'ok',
        ),
      error: (e) => this.notify(this.svc.describe(e), 'err'),
    });
  }

  cancel(rfq: RFQ): void {
    this.svc.cancelRFQ(rfq.id).subscribe({
      next: () => this.notify('RFQ cancelled', 'ok'),
      error: (e) => this.notify(this.svc.describe(e), 'err'),
    });
  }

  select(rfq: RFQ): void {
    this.selected.set(this.selected()?.id === rfq.id ? null : rfq);
  }

  /** Tenor in days, derived from the settlement dates the server returned. */
  termDays(rfq: RFQ): number {
    const ms = new Date(rfq.endDate).getTime() - new Date(rfq.startDate).getTime();
    return Math.max(1, Math.round(ms / 86_400_000));
  }

  /** Seconds left in the quote window, floored at zero. */
  countdown(rfq: RFQ): number {
    return Math.max(0, Math.floor((new Date(rfq.expiresAt).getTime() - Date.now()) / 1000));
  }

  stateClass(state: string): string {
    return `pill pill-${state.toLowerCase()}`;
  }

  private notify(text: string, kind: 'ok' | 'err'): void {
    this.toast.set({ text, kind });
    setTimeout(() => this.toast.set(null), 4000);
  }
}
