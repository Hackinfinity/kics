package Cx

import data.generic.common as common_lib

#default of mfa_delete is false
CxPolicy[result] {
	bucket := input.document[i].resource.aws_s3_bucket[name]
	not common_lib.valid_key(bucket, "versioning")

	result := {
		"documentId": input.document[i].id,
		"searchKey": sprintf("aws_s3_bucket[%s]", [name]),
		"issueType": "MissingAttribute",
		"keyExpectedValue": "'versioning' is equal 'true'",
		"keyActualValue": "'versioning' is missing",
	}
}

#default of enabled is false
CxPolicy[result] {
	bucket := input.document[i].resource.aws_s3_bucket[name]
	not common_lib.valid_key(bucket.versioning, "enabled")

	result := {
		"documentId": input.document[i].id,
		"searchKey": sprintf("aws_s3_bucket[%s].versioning", [name]),
		"issueType": "MissingAttribute",
		"keyExpectedValue": "'versioning.enabled' is equal 'true'",
		"keyActualValue": "'versioning.enabled' is missing",
	}
}

CxPolicy[result] {
	bucket := input.document[i].resource.aws_s3_bucket[name]
	bucket.versioning.enabled != true

	result := {
		"documentId": input.document[i].id,
		"searchKey": sprintf("aws_s3_bucket[%s].versioning.enabled", [name]),
		"issueType": "IncorrectValue",
		"keyExpectedValue": "'versioning.enabled' is equal 'true'",
		"keyActualValue": "'versioning.enabled' is equal 'false'",
	}
}
