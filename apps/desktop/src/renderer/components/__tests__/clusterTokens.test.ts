/**
 * Customer/environment grouping for the Select-Cluster screen family.
 *
 * splitLabel parses the pre-fields "Customer — Environment" naming convention,
 * and must NOT split ordinary hyphenated names. groupByCustomer buckets
 * endpoints by customer with the catch-all Ungrouped always last and named
 * customers in numeric-aware alphabetical order.
 */

import { describe, it, expect } from 'vitest';
import type { Endpoint } from '@ahv-remote-console/protocol';
import { splitLabel, groupByCustomer, UNGROUPED, userInitials } from '../clusterTokens';

function ep(partial: Partial<Endpoint> & { id: string; label: string }): Endpoint {
  return {
    plugin_id: 'nutanix',
    url: 'https://pc.example.com:9440',
    auth_methods: [],
    has_pinned_cert: false,
    allow_insecure_tls: false,
    ...partial,
  };
}

describe('splitLabel', () => {
  it('does NOT split an ordinary hyphenated name', () => {
    // No spaced separator → it is a single name, not "customer — environment".
    expect(splitLabel('prod-cluster-01')).toEqual({ customer: '', environment: '' });
  });

  it('splits on a spaced em dash', () => {
    expect(splitLabel('Acme — PROD')).toEqual({ customer: 'Acme', environment: 'PROD' });
  });

  it('splits on other explicit separators (en dash, pipe, slash, colon, spaced hyphen)', () => {
    expect(splitLabel('Acme – DR')).toEqual({ customer: 'Acme', environment: 'DR' });
    expect(splitLabel('Acme | STAGING')).toEqual({ customer: 'Acme', environment: 'STAGING' });
    expect(splitLabel('Acme / PROD')).toEqual({ customer: 'Acme', environment: 'PROD' });
    expect(splitLabel('Acme : PROD')).toEqual({ customer: 'Acme', environment: 'PROD' });
    expect(splitLabel('Acme - PROD')).toEqual({ customer: 'Acme', environment: 'PROD' });
  });

  it('does NOT split on an unspaced hyphen even with a trailing number', () => {
    expect(splitLabel('west-2')).toEqual({ customer: '', environment: '' });
  });

  it('returns empty for blank/whitespace input', () => {
    expect(splitLabel('')).toEqual({ customer: '', environment: '' });
    expect(splitLabel('   ')).toEqual({ customer: '', environment: '' });
  });
});

describe('groupByCustomer', () => {
  it('keeps Ungrouped (catch-all) last regardless of name', () => {
    const groups = groupByCustomer([
      ep({ id: '1', label: 'lonely-cluster' }), // no separator → Ungrouped
      ep({ id: '2', label: 'Acme — PROD' }),
    ]);
    expect(groups.map((g) => g.customer)).toEqual(['Acme', UNGROUPED]);
  });

  it('sorts named customers with numeric awareness (10 after 2, not before)', () => {
    const groups = groupByCustomer([
      ep({ id: 'a', label: 'Site-10 — PROD', tags: { customer: 'Site-10' } }),
      ep({ id: 'b', label: 'Site-2 — PROD', tags: { customer: 'Site-2' } }),
      ep({ id: 'c', label: 'Site-1 — PROD', tags: { customer: 'Site-1' } }),
    ]);
    expect(groups.map((g) => g.customer)).toEqual(['Site-1', 'Site-2', 'Site-10']);
  });

  it('uses the tags.customer field over the parsed label when present', () => {
    const groups = groupByCustomer([
      ep({ id: '1', label: 'anything', tags: { customer: 'Globex' } }),
    ]);
    expect(groups).toHaveLength(1);
    expect(groups[0]?.customer).toBe('Globex');
    expect(groups[0]?.endpoints.map((e) => e.id)).toEqual(['1']);
  });

  it('buckets every endpoint exactly once', () => {
    const groups = groupByCustomer([
      ep({ id: '1', label: 'Acme — PROD' }),
      ep({ id: '2', label: 'Acme — DR' }),
      ep({ id: '3', label: 'standalone' }),
    ]);
    const acme = groups.find((g) => g.customer === 'Acme');
    const ungrouped = groups.find((g) => g.customer === UNGROUPED);
    expect(acme?.endpoints.map((e) => e.id)).toEqual(['1', '2']);
    expect(ungrouped?.endpoints.map((e) => e.id)).toEqual(['3']);
  });
});

describe('userInitials', () => {
  it('uses the first letters of a two-word display name', () => {
    expect(userInitials({ username: 'jane.doe@example.com', display_name: 'Jane Doe' })).toBe(
      'JD',
    );
  });

  it('uses the first two characters of a hyphenated username local part (NOT one per segment)', () => {
    // "si-ai" must read "SI", not "SA" — usernames are opaque identifiers,
    // not "first-name last-name" pairs.
    expect(userInitials({ username: 'si-ai@example.com' })).toBe('SI');
  });

  it('strips the @domain before deriving initials', () => {
    expect(userInitials({ username: 'admin@cluster.local' })).toBe('AD');
  });

  it('uses a single-word display name over the username', () => {
    expect(userInitials({ username: 'x1@y', display_name: 'Administrator' })).toBe('AD');
  });

  it('returns null without an identity or with an empty one', () => {
    expect(userInitials(null)).toBeNull();
    expect(userInitials(undefined)).toBeNull();
    expect(userInitials({ username: '' })).toBeNull();
  });
});
