// Code generated by smithy-go-codegen DO NOT EDIT.

package iam

import (
	"context"
	"fmt"
	awsmiddleware "github.com/aws/aws-sdk-go-v2/aws/middleware"
	"github.com/aws/smithy-go/middleware"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

// Deletes the specified service-specific credential.
func (c *Client) DeleteServiceSpecificCredential(ctx context.Context, params *DeleteServiceSpecificCredentialInput, optFns ...func(*Options)) (*DeleteServiceSpecificCredentialOutput, error) {
	if params == nil {
		params = &DeleteServiceSpecificCredentialInput{}
	}

	result, metadata, err := c.invokeOperation(ctx, "DeleteServiceSpecificCredential", params, optFns, c.addOperationDeleteServiceSpecificCredentialMiddlewares)
	if err != nil {
		return nil, err
	}

	out := result.(*DeleteServiceSpecificCredentialOutput)
	out.ResultMetadata = metadata
	return out, nil
}

type DeleteServiceSpecificCredentialInput struct {

	// The unique identifier of the service-specific credential. You can get this
	// value by calling ListServiceSpecificCredentials . This parameter allows (through
	// its regex pattern (http://wikipedia.org/wiki/regex) ) a string of characters
	// that can consist of any upper or lowercased letter or digit.
	//
	// This member is required.
	ServiceSpecificCredentialId *string

	// The name of the IAM user associated with the service-specific credential. If
	// this value is not specified, then the operation assumes the user whose
	// credentials are used to call the operation. This parameter allows (through its
	// regex pattern (http://wikipedia.org/wiki/regex) ) a string of characters
	// consisting of upper and lowercase alphanumeric characters with no spaces. You
	// can also include any of the following characters: _+=,.@-
	UserName *string

	noSmithyDocumentSerde
}

type DeleteServiceSpecificCredentialOutput struct {
	// Metadata pertaining to the operation's result.
	ResultMetadata middleware.Metadata

	noSmithyDocumentSerde
}

func (c *Client) addOperationDeleteServiceSpecificCredentialMiddlewares(stack *middleware.Stack, options Options) (err error) {
	if err := stack.Serialize.Add(&setOperationInputMiddleware{}, middleware.After); err != nil {
		return err
	}
	err = stack.Serialize.Add(&awsAwsquery_serializeOpDeleteServiceSpecificCredential{}, middleware.After)
	if err != nil {
		return err
	}
	err = stack.Deserialize.Add(&awsAwsquery_deserializeOpDeleteServiceSpecificCredential{}, middleware.After)
	if err != nil {
		return err
	}
	if err := addProtocolFinalizerMiddlewares(stack, options, "DeleteServiceSpecificCredential"); err != nil {
		return fmt.Errorf("add protocol finalizers: %v", err)
	}

	if err = addlegacyEndpointContextSetter(stack, options); err != nil {
		return err
	}
	if err = addSetLoggerMiddleware(stack, options); err != nil {
		return err
	}
	if err = addClientRequestID(stack); err != nil {
		return err
	}
	if err = addComputeContentLength(stack); err != nil {
		return err
	}
	if err = addResolveEndpointMiddleware(stack, options); err != nil {
		return err
	}
	if err = addComputePayloadSHA256(stack); err != nil {
		return err
	}
	if err = addRetry(stack, options); err != nil {
		return err
	}
	if err = addRawResponseToMetadata(stack); err != nil {
		return err
	}
	if err = addRecordResponseTiming(stack); err != nil {
		return err
	}
	if err = addClientUserAgent(stack, options); err != nil {
		return err
	}
	if err = smithyhttp.AddErrorCloseResponseBodyMiddleware(stack); err != nil {
		return err
	}
	if err = smithyhttp.AddCloseResponseBodyMiddleware(stack); err != nil {
		return err
	}
	if err = addSetLegacyContextSigningOptionsMiddleware(stack); err != nil {
		return err
	}
	if err = addOpDeleteServiceSpecificCredentialValidationMiddleware(stack); err != nil {
		return err
	}
	if err = stack.Initialize.Add(newServiceMetadataMiddleware_opDeleteServiceSpecificCredential(options.Region), middleware.Before); err != nil {
		return err
	}
	if err = addRecursionDetection(stack); err != nil {
		return err
	}
	if err = addRequestIDRetrieverMiddleware(stack); err != nil {
		return err
	}
	if err = addResponseErrorMiddleware(stack); err != nil {
		return err
	}
	if err = addRequestResponseLogging(stack, options); err != nil {
		return err
	}
	if err = addDisableHTTPSMiddleware(stack, options); err != nil {
		return err
	}
	return nil
}

func newServiceMetadataMiddleware_opDeleteServiceSpecificCredential(region string) *awsmiddleware.RegisterServiceMetadata {
	return &awsmiddleware.RegisterServiceMetadata{
		Region:        region,
		ServiceID:     ServiceID,
		OperationName: "DeleteServiceSpecificCredential",
	}
}