# Least-privilege IAM examples

Replace `BUCKET` and `PREFIX`; for a root repository use `.git/git3/*` directly. Bucket-list access
is deliberately absent from reader and writer roles.

Reader object action:

```json
{"Effect":"Allow","Action":["s3:GetObject"],"Resource":"arn:aws:s3:::BUCKET/PREFIX/.git/git3/*"}
```

Writer/maintainer object actions:

```json
{"Effect":"Allow","Action":["s3:GetObject","s3:PutObject","s3:AbortMultipartUpload"],"Resource":"arn:aws:s3:::BUCKET/PREFIX/.git/git3/*"}
```

GC operators additionally need object deletion and prefix-scoped listing:

```json
[
  {"Effect":"Allow","Action":["s3:GetObject","s3:PutObject","s3:DeleteObject"],"Resource":"arn:aws:s3:::BUCKET/PREFIX/.git/git3/*"},
  {"Effect":"Allow","Action":["s3:ListBucket"],"Resource":"arn:aws:s3:::BUCKET","Condition":{"StringLike":{"s3:prefix":"PREFIX/.git/git3/*"}}}
]
```

SSE-KMS users also need the applicable `kms:Encrypt`, `kms:Decrypt`, `kms:GenerateDataKey`, and key
policy permissions. Conditional replacement requires both get and put access to HEAD.
