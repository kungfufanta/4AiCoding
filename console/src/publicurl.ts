// Decides whether the console should point out that previews are not reachable
// from the internet. True for LAN-only preview domains (localhost, private IPs,
// sslip.io/nip.io wrapping a private IP, .local/.lan/.internal) and for
// IP-based domains without TLS. False as soon as a real domain is configured.

const PRIVATE_IP = /^(10\.|127\.|192\.168\.|172\.(1[6-9]|2\d|3[01])\.|100\.(6[4-9]|[7-9]\d|1[01]\d|12[0-7])\.|169\.254\.)/
const IP = /^\d{1,3}(\.\d{1,3}){3}$/
const MAGIC_DNS = /^(?:(\d{1,3})[.-](\d{1,3})[.-](\d{1,3})[.-](\d{1,3}))\.(sslip\.io|nip\.io)$/i

export type PreviewReach = 'lan' | 'no-tls' | 'ok'

export function previewReach(previewDomain: string, tls: boolean): PreviewReach {
  const d = (previewDomain || '').trim().toLowerCase()
  if (!d || d === 'localhost' || d.endsWith('.localhost') || /\.(local|lan|internal|home|home\.arpa)$/.test(d)) return 'lan'
  if (IP.test(d)) return PRIVATE_IP.test(d) ? 'lan' : tls ? 'ok' : 'no-tls'
  const m = d.match(MAGIC_DNS)
  if (m) {
    const ip = `${m[1]}.${m[2]}.${m[3]}.${m[4]}`
    return PRIVATE_IP.test(ip) ? 'lan' : tls ? 'ok' : 'no-tls'
  }
  return 'ok'
}
