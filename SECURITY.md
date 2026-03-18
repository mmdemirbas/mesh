# Security Policy

## Reporting a Vulnerability

If you discover a security vulnerability in mesh, please report it responsibly.

**Do NOT open a public GitHub issue for security vulnerabilities.**

Instead, please email: **security@mmdemirbas.com** (or open a private security advisory on GitHub).

Include:
- Description of the vulnerability
- Steps to reproduce
- Potential impact
- Suggested fix (if any)

You will receive an acknowledgment within 48 hours and a detailed response within 7 days.

## Scope

The following are in scope for security reports:
- SSH server/client authentication bypass
- Remote code execution
- Path traversal / file access outside intended directories
- Credential exposure (passwords, keys, tokens in logs or API)
- Denial of service via resource exhaustion
- Clipboard data interception or manipulation
- Privilege escalation

## Known Limitations

- **Clipsync uses unencrypted HTTP** for clipboard sync between peers. This is documented and planned for TLS migration (see ROADMAP.md). Use only on trusted networks until TLS is implemented.
- **StrictHostKeyChecking=no** disables SSH server identity verification. This is an explicit opt-in that is logged as a warning.

## Security Design Principles

- SSH keys and agent-based auth are preferred over passwords
- Passwords never appear in config files (use `password_command` to fetch from external tools)
- Config files with sensitive directives warn if world-readable (permission check on load)
- Admin API binds to localhost only
- All file operations sanitize paths against traversal
- Rate limiting on SSH server authentication
- Handshake timeouts on all protocol handlers (SSH, SOCKS5, HTTP proxy)
- Bounded peer discovery (max 32 dynamic peers) to prevent OOM attacks
