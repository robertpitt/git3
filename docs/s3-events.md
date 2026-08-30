# Triggering builds from S3 object events

Amazon S3 Event Notifications can turn a git3 publication into an event-driven build. A typical
flow is:

```text
git push -> S3 .git/git3/HEAD -> Lambda -> CodeBuild -> git clone s3://BUCKET/PREFIX
```

This guide uses Lambda as a small adapter because S3 cannot start CodeBuild directly. Lambda checks
that the event is for the expected git3 repository and calls `StartBuild`; CodeBuild then clones the
latest published repository state with git3.

## Choose the publication event

Configure the notification for the repository's git3 `HEAD` object:

```text
<repository-prefix>/.git/git3/HEAD
```

For `s3://my-bucket/repos/example`, the object key is:

```text
repos/example/.git/git3/HEAD
```

For a repository at the bucket root, it is `.git/git3/HEAD`.

The `HEAD` write is git3's publication point. Its referenced packs and transaction records are
uploaded first, so reacting to their individual object-created events can start a build before a
push has been published. Filtering for `HEAD` also avoids starting a build for every pack or
metadata object in one push.

Use the `s3:ObjectCreated:Put` event type. git3 writes `HEAD` with a single conditional `PutObject`,
including for the first publication. An S3 filter can narrow delivery with:

- prefix: `repos/example/.git/git3/`
- suffix: `HEAD`

S3 prefix and suffix filters do not provide an exact-key match, so the Lambda function below also
compares the decoded key with an exact expected value.

## Create the CodeBuild project

Create a CodeBuild project with source type **No source**. The buildspec must be defined inline on
the project or stored separately in S3: it cannot come from the git3 repository because cloning that
repository is the first build step.

Set these project environment variables:

| Name | Example | Purpose |
| --- | --- | --- |
| `GIT3_REMOTE_URL` | `s3://my-bucket/repos/example` | Repository to clone |
| `GIT3_VERSION` | A release tag such as `v1.2.3` | Pins the git3 release used by the build |

CodeBuild supplies credentials from its service role through the standard AWS SDK credential
chain. Do not put long-lived AWS access keys in project environment variables.

Use a buildspec like this and replace the final commands with the repository's build or deployment
entry point:

```yaml
version: 0.2

phases:
  install:
    commands:
      - curl -fsSL https://github.com/robertpitt/git3/releases/latest/download/install.sh | sh
      - export PATH="$HOME/.local/bin:$PATH"
      - git3 version
  pre_build:
    commands:
      - git clone "$GIT3_REMOTE_URL" repository
  build:
    commands:
      - cd repository && ./ci/build.sh
```

The installer honors `GIT3_VERSION`, verifies the release checksum, and installs git3 and its Git
remote-helper names together. Pinning the version makes builds reproducible. A build environment
without public internet access should instead include the pinned git3 binary in its image or fetch
the release artifacts through an approved internal mirror.

The CodeBuild service role needs read access to the git3 repository objects:

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Sid": "ReadGit3Repository",
      "Effect": "Allow",
      "Action": ["s3:GetObject"],
      "Resource": "arn:aws:s3:::my-bucket/repos/example/.git/git3/*"
    }
  ]
}
```

Add the normal CodeBuild CloudWatch Logs permissions to that role. For SSE-KMS objects, also grant
`kms:Decrypt` on the applicable KMS key and allow the role in the key policy. Cloning does not need
S3 list, put, or delete permission.

For an S3-compatible service rather than Amazon S3, also configure `GIT3_ENDPOINT` and, when the
service requires it, `GIT3_PATH_STYLE=true` on the CodeBuild project. The service must be reachable
from the build environment. The notification setup in this guide is specific to Amazon S3; a custom
service needs an equivalent object-event or webhook mechanism to invoke the adapter.

## Create the Lambda adapter

Create a Python Lambda function with these environment variables:

| Name | Example |
| --- | --- |
| `CODEBUILD_PROJECT_NAME` | `example-build` |
| `EXPECTED_BUCKET` | `my-bucket` |
| `EXPECTED_HEAD_KEY` | `repos/example/.git/git3/HEAD` |

Use this handler:

```python
import hashlib
import os
from urllib.parse import unquote_plus

import boto3


codebuild = boto3.client("codebuild")
project_name = os.environ["CODEBUILD_PROJECT_NAME"]
expected_bucket = os.environ["EXPECTED_BUCKET"]
expected_head_key = os.environ["EXPECTED_HEAD_KEY"]


def handler(event, context):
    build_ids = []

    for record in event.get("Records", []):
        if record.get("eventSource") != "aws:s3":
            continue
        if not record.get("eventName", "").startswith("ObjectCreated:"):
            continue

        bucket = record["s3"]["bucket"]["name"]
        key = unquote_plus(record["s3"]["object"]["key"])
        if bucket != expected_bucket or key != expected_head_key:
            continue

        sequencer = record["s3"]["object"].get("sequencer", "")
        token_source = f"{bucket}\0{key}\0{sequencer}".encode()
        idempotency_token = hashlib.sha256(token_source).hexdigest()

        result = codebuild.start_build(
            projectName=project_name,
            idempotencyToken=idempotency_token,
            environmentVariablesOverride=[
                {
                    "name": "GIT3_EVENT_SEQUENCER",
                    "value": sequencer,
                    "type": "PLAINTEXT",
                }
            ],
        )
        build_ids.append(result["build"]["id"])

    return {"startedBuildIds": build_ids}
```

The token suppresses a repeated `StartBuild` request for the same event within CodeBuild's
idempotency window. The build itself should still be safe to repeat because S3 notifications use
at-least-once delivery and a duplicate can arrive after that window.

Attach the AWS-managed `AWSLambdaBasicExecutionRole` policy, or equivalent logging permissions, to
the Lambda execution role. Add this project-scoped permission:

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Sid": "StartRepositoryBuild",
      "Effect": "Allow",
      "Action": "codebuild:StartBuild",
      "Resource": "arn:aws:codebuild:REGION:ACCOUNT_ID:project/example-build"
    }
  ]
}
```

The Lambda role does not need S3 read access for this basic adapter; the bucket and key arrive in
the event and the CodeBuild role performs the clone.

## Connect S3 to Lambda

The S3 bucket and Lambda function must be in the same AWS Region. First allow the bucket to invoke
the function, restricting the permission to the bucket and account:

```sh
aws lambda add-permission \
  --function-name git3-start-build \
  --statement-id allow-git3-bucket \
  --action lambda:InvokeFunction \
  --principal s3.amazonaws.com \
  --source-arn arn:aws:s3:::my-bucket \
  --source-account ACCOUNT_ID
```

Then add an event notification to the bucket with:

- destination: the Lambda function;
- event type: `s3:ObjectCreated:Put`;
- prefix: `repos/example/.git/git3/`; and
- suffix: `HEAD`.

Adding or replacing a bucket notification configuration affects the bucket's complete notification
configuration. Infrastructure-as-code should merge this destination with any existing destinations
instead of overwriting them. S3 also rejects overlapping prefix/suffix filters for the same event
types, so review existing notification rules first.

## Test the flow

Push a real commit rather than writing the `HEAD` object manually:

```sh
git push origin main
```

Then check the Lambda logs for the returned CodeBuild build ID and inspect that build's logs. The
clone should contain the latest successfully published refs. Never create, copy, or edit
`.git/git3/HEAD` outside git3; direct writes can corrupt the repository protocol.

## Delivery and concurrency behavior

S3 notifications are usually delivered in seconds, but they are not a strict real-time or exactly
once stream. Design the downstream build with these properties in mind:

- **At least once:** make builds and deployments idempotent. Use DynamoDB or another durable store
  keyed by the S3 sequencer if duplicate suppression must last longer than CodeBuild's idempotency
  window.
- **Not ordered:** a build always clones the current repository state when it starts, which may be
  newer than the push that produced its event. The S3 event is a wake-up signal, not a snapshot pin.
- **No branch filter:** S3 sees the repository-level `HEAD` publication, not the changed Git refs.
  To trigger only for a particular branch, the adapter must read and validate the published git3
  metadata or the build must decide whether the current branch tip needs processing.
- **Bursts:** several fast pushes can start overlapping builds. Put SQS or EventBridge between S3
  and the starter when builds must be buffered, retried with a dead-letter queue, or coalesced.
- **All HEAD publications:** maintenance and garbage-collection barriers can also replace git3
  `HEAD` without changing refs. If builds must run only for ref-changing pushes, have the adapter
  read and validate `HEAD` and durably compare its `logicalGeneration` and `transactionId` with the
  last processed values.
- **Feedback loops:** keep build output outside the watched key. A job that pushes back to the same
  git3 repository will intentionally trigger another event and needs an explicit loop guard.

For the AWS behavior used here, see the AWS documentation for [S3 Event
Notifications](https://docs.aws.amazon.com/AmazonS3/latest/userguide/EventNotifications.html),
[S3 notification key
filtering](https://docs.aws.amazon.com/AmazonS3/latest/userguide/notification-how-to-filtering.html),
[using S3 with Lambda](https://docs.aws.amazon.com/lambda/latest/dg/with-s3.html), and the CodeBuild
[`StartBuild` API](https://docs.aws.amazon.com/codebuild/latest/APIReference/API_StartBuild.html).
