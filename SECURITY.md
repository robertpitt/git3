# Security policy

Report vulnerabilities privately through GitHub Security Advisories for this repository. Do not
include live credentials, signed URLs, customer bucket names, or repository contents in a report.
Supported release lines and response targets are published on the Security Advisories page.

AWS credentials, IAM, bucket/KMS policy, and TLS are the security boundary. Anyone able to write
`.git/git3/HEAD` is trusted as a repository administrator. Git LFS payloads below
`.git/git3/lfs/objects/` are untrusted until their SHA-256 OID and size validate. git3 never accepts
credentials in its URL or copies them into Git configuration and redacts authorization material
from diagnostics.
