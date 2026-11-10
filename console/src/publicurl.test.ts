import { describe, it, expect } from 'vitest'
import { previewReach } from './publicurl'

describe('previewReach', () => {
  it('flags LAN-only domains', () => {
    for (const d of ['', 'localhost', 'app.localhost', 'box.local', 'nas.lan', '192.168.1.20', '10.0.0.5', '172.20.3.1', '100.90.1.1',
      '192.168.1.20.sslip.io', '10-0-0-5.nip.io', '127.0.0.1.sslip.io']) {
      expect(previewReach(d, false), d).toBe('lan')
    }
  })
  it('flags public IP-based domains without TLS, accepts them with TLS', () => {
    expect(previewReach('147.224.191.9.sslip.io', false)).toBe('no-tls')
    expect(previewReach('147.224.191.9', false)).toBe('no-tls')
    expect(previewReach('147.224.191.9.sslip.io', true)).toBe('ok')
  })
  it('never nags on real domains or a tunnel', () => {
    expect(previewReach('previews.example.com', false)).toBe('ok')
    expect(previewReach('acme.sandboxd.io', true)).toBe('ok')
  })
})
