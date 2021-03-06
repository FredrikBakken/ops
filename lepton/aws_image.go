package lepton

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/request"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/olekukonko/tablewriter"
)

// BuildImage to be upload on AWS
func (p *AWS) BuildImage(ctx *Context) (string, error) {
	c := ctx.config
	err := BuildImage(*c)
	if err != nil {
		return "", err
	}

	return p.CustomizeImage(ctx)
}

// BuildImageWithPackage to upload on AWS
func (p *AWS) BuildImageWithPackage(ctx *Context, pkgpath string) (string, error) {
	c := ctx.config
	err := BuildImageFromPackage(pkgpath, *c)
	if err != nil {
		return "", err
	}
	return p.CustomizeImage(ctx)
}

// CreateImage - Creates image on AWS using nanos images
// TODO : re-use and cache DefaultClient and instances.
func (p *AWS) CreateImage(ctx *Context, imagePath string) error {
	// this is a really convulted setup
	// 1) upload the image
	// 2) create a snapshot
	// 3) create an image

	err := p.Storage.CopyToBucket(ctx.config, imagePath)
	if err != nil {
		return err
	}

	c := ctx.config

	bucket := c.CloudConfig.BucketName
	key := c.CloudConfig.ImageName

	input := &ec2.ImportSnapshotInput{
		Description: aws.String("NanoVMs test"),
		DiskContainer: &ec2.SnapshotDiskContainer{
			Description: aws.String("NanoVMs test"),
			Format:      aws.String("raw"),
			UserBucket: &ec2.UserBucket{
				S3Bucket: aws.String(bucket),
				S3Key:    aws.String(key),
			},
		},
	}

	ctx.logger.Info("Importing snapshot from s3 image file")
	res, err := p.ec2.ImportSnapshot(input)
	if err != nil {
		return err
	}

	snapshotID, err := p.waitSnapshotToBeReady(c, res.ImportTaskId)
	if err != nil {
		return err
	}

	// delete the tmp s3 image
	ctx.logger.Info("Deleting s3 image file")
	err = p.Storage.DeleteFromBucket(c, key)
	if err != nil {
		return err
	}

	// tag the volume
	tags, _ := buildAwsTags(c.RunConfig.Tags, key)

	ctx.logger.Log("Tagging snapshot")
	_, err = p.ec2.CreateTags(&ec2.CreateTagsInput{
		Resources: []*string{snapshotID},
		Tags:      tags,
	})
	if err != nil {
		return err
	}

	t := time.Now().UnixNano()
	s := strconv.FormatInt(t, 10)

	amiName := key + s

	// register ami
	rinput := &ec2.RegisterImageInput{
		Name:         aws.String(amiName),
		Architecture: aws.String("x86_64"),
		BlockDeviceMappings: []*ec2.BlockDeviceMapping{
			{
				DeviceName: aws.String("/dev/sda1"),
				Ebs: &ec2.EbsBlockDevice{
					DeleteOnTermination: aws.Bool(false),
					SnapshotId:          snapshotID,
					VolumeType:          aws.String("gp2"),
				},
			},
		},
		Description:        aws.String(fmt.Sprintf("nanos image %s", key)),
		RootDeviceName:     aws.String("/dev/sda1"),
		VirtualizationType: aws.String("hvm"),
		EnaSupport:         aws.Bool(false),
	}

	ctx.logger.Info("Registering image")
	resreg, err := p.ec2.RegisterImage(rinput)
	if err != nil {
		return err
	}

	// Add name tag to the created ami
	ctx.logger.Info("Tagging image")
	_, err = p.ec2.CreateTags(&ec2.CreateTagsInput{
		Resources: []*string{resreg.ImageId},
		Tags:      tags,
	})

	return nil
}

func getAWSImages(ec2Service *ec2.EC2) (*ec2.DescribeImagesOutput, error) {
	filters := []*ec2.Filter{{Name: aws.String("tag:CreatedBy"), Values: aws.StringSlice([]string{"ops"})}}

	input := &ec2.DescribeImagesInput{
		Owners: []*string{
			aws.String("self"),
		},
		Filters: filters,
	}

	result, err := ec2Service.DescribeImages(input)
	if err != nil {
		if aerr, ok := err.(awserr.Error); ok {
			switch aerr.Code() {
			default:
				return nil, errors.New(aerr.Error())
			}
		} else {
			return nil, errors.New(err.Error())
		}
	}

	return result, nil
}

// GetImages return all images on AWS
func (p *AWS) GetImages(ctx *Context) ([]CloudImage, error) {
	var cimages []CloudImage

	result, err := getAWSImages(p.ec2)
	if err != nil {
		return nil, err
	}

	images := result.Images
	for _, image := range images {
		var name string
		if image.Tags != nil {
			name = aws.StringValue(image.Tags[0].Value)
		} else {
			name = "n/a"
		}

		cimage := CloudImage{
			Name:    name,
			ID:      *image.Name,
			Status:  *image.State,
			Created: *image.CreationDate,
		}

		cimages = append(cimages, cimage)
	}

	return cimages, nil
}

// ListImages lists images on AWS
func (p *AWS) ListImages(ctx *Context) error {
	cimages, err := p.GetImages(ctx)
	if err != nil {
		return err
	}

	table := tablewriter.NewWriter(os.Stdout)
	table.SetHeader([]string{"Name", "Id", "Status", "Created"})
	table.SetHeaderColor(
		tablewriter.Colors{tablewriter.Bold, tablewriter.FgCyanColor},
		tablewriter.Colors{tablewriter.Bold, tablewriter.FgCyanColor},
		tablewriter.Colors{tablewriter.Bold, tablewriter.FgCyanColor},
		tablewriter.Colors{tablewriter.Bold, tablewriter.FgCyanColor})
	table.SetRowLine(true)

	for _, image := range cimages {
		var row []string

		row = append(row, image.Name)
		row = append(row, image.ID)
		row = append(row, image.Status)
		row = append(row, image.Created)

		table.Append(row)
	}

	table.Render()

	return nil
}

// ResizeImage is not supported on AWS.
func (p *AWS) ResizeImage(ctx *Context, imagename string, hbytes string) error {
	return fmt.Errorf("Operation not supported")
}

// DeleteImage deletes image from AWS by ami name
func (p *AWS) DeleteImage(ctx *Context, imagename string) error {
	// delete ami by ami name
	svc, err := session.NewSession(&aws.Config{
		Region: aws.String(ctx.config.CloudConfig.Zone)},
	)
	compute := ec2.New(svc)

	ec2Filters := []*ec2.Filter{}
	vals := []string{imagename}
	ec2Filters = append(ec2Filters, &ec2.Filter{Name: aws.String("name"), Values: aws.StringSlice(vals)})

	input := &ec2.DescribeImagesInput{
		Filters: ec2Filters,
	}

	result, err := compute.DescribeImages(input)
	if err != nil {
		if aerr, ok := err.(awserr.Error); ok {
			switch aerr.Code() {
			default:
				fmt.Println(aerr.Error())
			}
		} else {
			fmt.Println(err.Error())
		}
		return err
	}
	if len(result.Images) == 0 {
		return fmt.Errorf("Error running deregister image operation: image %v not found", imagename)
	}

	amiID := aws.StringValue(result.Images[0].ImageId)
	snapID := aws.StringValue(result.Images[0].BlockDeviceMappings[0].Ebs.SnapshotId)

	// grab snapshotid && grab image id

	params := &ec2.DeregisterImageInput{
		ImageId: aws.String(amiID),
		DryRun:  aws.Bool(false),
	}
	_, err = compute.DeregisterImage(params)
	if err != nil {
		return fmt.Errorf("Error running deregister image operation: %s", err)
	}

	// DeleteSnapshot
	params2 := &ec2.DeleteSnapshotInput{
		SnapshotId: aws.String(snapID),
		DryRun:     aws.Bool(false),
	}
	_, err = compute.DeleteSnapshot(params2)
	if err != nil {
		return fmt.Errorf("Error running snapshot delete: %s", err)
	}

	return nil
}

// SyncImage syncs image from provider to another provider
func (p *AWS) SyncImage(config *Config, target Provider, image string) error {
	fmt.Println("not yet implemented")
	return nil
}

// CustomizeImage returns image path with adaptations needed by cloud provider
func (p *AWS) CustomizeImage(ctx *Context) (string, error) {
	imagePath := ctx.config.RunConfig.Imagename
	return imagePath, nil
}

// not an archive - just raw disk image
func (p *AWS) getArchiveName(ctx *Context) string {
	imagePath := ctx.config.RunConfig.Imagename
	return imagePath
}

func (p *AWS) waitSnapshotToBeReady(config *Config, importTaskID *string) (*string, error) {
	taskFilter := &ec2.DescribeImportSnapshotTasksInput{
		ImportTaskIds: []*string{importTaskID},
	}

	_, err := p.ec2.DescribeImportSnapshotTasks(taskFilter)
	if err != nil {
		return nil, err
	}

	fmt.Println("waiting for snapshot - can take like 5min.... ")

	waitStartTime := time.Now()

	ct := aws.BackgroundContext()
	w := request.Waiter{
		Name:        "DescribeImportSnapshotTasks",
		Delay:       request.ConstantWaiterDelay(15 * time.Second),
		MaxAttempts: 120,
		Acceptors: []request.WaiterAcceptor{
			{
				State:    request.SuccessWaiterState,
				Matcher:  request.PathAllWaiterMatch,
				Argument: "ImportSnapshotTasks[].SnapshotTaskDetail.Status",
				Expected: "completed",
			},
			{
				State:    request.FailureWaiterState,
				Matcher:  request.PathAnyWaiterMatch,
				Argument: "ImportSnapshotTasks[].SnapshotTaskDetail.Status",
				Expected: "deleted",
			},
			{
				State:    request.FailureWaiterState,
				Matcher:  request.PathAnyWaiterMatch,
				Argument: "ImportSnapshotTasks[].SnapshotTaskDetail.Status",
				Expected: "deleting",
			},
		},
		NewRequest: func(opts []request.Option) (*request.Request, error) {
			req, _ := p.ec2.DescribeImportSnapshotTasksRequest(taskFilter)
			req.SetContext(ct)
			req.ApplyOptions(opts...)
			return req, nil
		},
	}
	err = w.WaitWithContext(ct)
	if err != nil {
		fmt.Printf("import timed out after %f minutes\n", time.Since(waitStartTime).Minutes())
		return nil, err
	}

	fmt.Printf("import done - took %f minutes\n", time.Since(waitStartTime).Minutes())

	describeOutput, err := p.ec2.DescribeImportSnapshotTasks(taskFilter)
	if err != nil {
		return nil, err
	}

	snapshotID := describeOutput.ImportSnapshotTasks[0].SnapshotTaskDetail.SnapshotId

	return snapshotID, nil
}
