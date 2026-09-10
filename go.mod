module fma

go 1.25.0

require (
	github.com/Jabberwocky238/go-pop3 v0.1.6
	github.com/aws/aws-sdk-go-v2 v1.41.5
	github.com/aws/aws-sdk-go-v2/credentials v1.17.64
	github.com/aws/aws-sdk-go-v2/service/s3 v1.97.3
	github.com/aws/smithy-go v1.24.2
	github.com/emersion/go-imap v1.2.1
	github.com/emersion/go-imap/v2 v2.0.0-beta.8
	github.com/emersion/go-message v0.18.2
	github.com/emersion/go-sasl v0.0.0-20241020182733-b788ff22d5a6
	github.com/emersion/go-smtp v0.25.0
	github.com/klauspost/compress v1.20.0
	github.com/naust-mail/naust-jmap/core v0.4.2
	github.com/naust-mail/naust-jmap/datatypes/mail v0.3.3
)

require (
	github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream v1.7.8 // indirect
	github.com/aws/aws-sdk-go-v2/internal/configsources v1.4.21 // indirect
	github.com/aws/aws-sdk-go-v2/internal/endpoints/v2 v2.7.21 // indirect
	github.com/aws/aws-sdk-go-v2/internal/v4a v1.4.22 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/accept-encoding v1.13.7 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/checksum v1.9.13 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/presigned-url v1.13.21 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/s3shared v1.19.21 // indirect
	golang.org/x/text v0.39.0 // indirect
)

replace github.com/emersion/go-smtp => github.com/Jabberwocky238/go-smtp v0.25.1-0.20260910174640-b0673510e580

replace github.com/naust-mail/naust-jmap/datatypes/mail => github.com/Jabberwocky238/naust-jmap/datatypes/mail v0.3.4-0.20260910204519-ea6016819f55
