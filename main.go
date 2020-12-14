package main

import (
	"fmt"
	"os"

	"github.com/pulumi/pulumi-aws/sdk/v3/go/aws/ec2"
	"github.com/pulumi/pulumi-aws/sdk/v3/go/aws/ecs"
	elb "github.com/pulumi/pulumi-aws/sdk/v3/go/aws/elasticloadbalancingv2"
	"github.com/pulumi/pulumi-aws/sdk/v3/go/aws/iam"
	"github.com/pulumi/pulumi-docker/sdk/v2/go/docker"
	"github.com/pulumi/pulumi/sdk/v2/go/pulumi"
)

func main() {
	pulumi.Run(func(ctx *pulumi.Context) error {
		// Read back the default VPC and public subnets, which we will use.
		t := true
		vpc, err := ec2.LookupVpc(ctx, &ec2.LookupVpcArgs{Default: &t})
		if err != nil {
			return err
		}
		subnet, err := ec2.GetSubnetIds(ctx, &ec2.GetSubnetIdsArgs{VpcId: vpc.Id})
		if err != nil {
			return err
		}

		// Create a SecurityGroup that permits HTTP ingress and unrestricted egress.
		webSg, err := ec2.NewSecurityGroup(ctx, "web-sg", &ec2.SecurityGroupArgs{
			VpcId: pulumi.String(vpc.Id),
			Egress: ec2.SecurityGroupEgressArray{
				ec2.SecurityGroupEgressArgs{
					Protocol:   pulumi.String("-1"),
					FromPort:   pulumi.Int(0),
					ToPort:     pulumi.Int(0),
					CidrBlocks: pulumi.StringArray{pulumi.String("0.0.0.0/0")},
				},
			},
			Ingress: ec2.SecurityGroupIngressArray{
				ec2.SecurityGroupIngressArgs{
					Protocol:   pulumi.String("tcp"),
					FromPort:   pulumi.Int(80),
					ToPort:     pulumi.Int(80),
					CidrBlocks: pulumi.StringArray{pulumi.String("0.0.0.0/0")},
				},
			},
		})
		if err != nil {
			return err
		}

		// Create an ECS cluster to run a container-based service.
		cluster, err := ecs.NewCluster(ctx, "app-cluster", nil)
		if err != nil {
			return err
		}

		// Create an IAM role that can be used by our service's task.
		taskExecRole, err := iam.NewRole(ctx, "task-exec-role", &iam.RoleArgs{
			AssumeRolePolicy: pulumi.String(`{
    "Version": "2008-10-17",
    "Statement": [{
        "Sid": "",
        "Effect": "Allow",
        "Principal": {
            "Service": "ecs-tasks.amazonaws.com"
        },
        "Action": "sts:AssumeRole"
    }]
}`),
		})
		if err != nil {
			return err
		}
		_, err = iam.NewRolePolicyAttachment(ctx, "task-exec-policy", &iam.RolePolicyAttachmentArgs{
			Role:      taskExecRole.Name,
			PolicyArn: pulumi.String("arn:aws:iam::aws:policy/service-role/AmazonECSTaskExecutionRolePolicy"),
		})
		if err != nil {
			return err
		}

		// Create a load balancer to listen for HTTP traffic on port 80.
		webLb, err := elb.NewLoadBalancer(ctx, "web-lb", &elb.LoadBalancerArgs{
			Subnets:        toPulumiStringArray(subnet.Ids),
			SecurityGroups: pulumi.StringArray{webSg.ID().ToStringOutput()},
		})
		if err != nil {
			return err
		}
		webTg, err := elb.NewTargetGroup(ctx, "web-tg", &elb.TargetGroupArgs{
			Port:       pulumi.Int(80),
			Protocol:   pulumi.String("HTTP"),
			TargetType: pulumi.String("ip"),
			VpcId:      pulumi.String(vpc.Id),
		})
		if err != nil {
			return err
		}
		webListener, err := elb.NewListener(ctx, "web-listener", &elb.ListenerArgs{
			LoadBalancerArn: webLb.Arn,
			Port:            pulumi.Int(80),
			DefaultActions: elb.ListenerDefaultActionArray{
				elb.ListenerDefaultActionArgs{
					Type:           pulumi.String("forward"),
					TargetGroupArn: webTg.Arn,
				},
			},
		})
		if err != nil {
			return err
		}

		githubActor := pulumi.String(os.Getenv("GITHUB_ACTOR"))
		fmt.Println("Github Actor: {}", githubActor)
		githubToken := pulumi.String(os.Getenv("GITHUB_TOKEN"))
		_, err = docker.NewProvider(ctx, "github-docker", &docker.ProviderArgs{
			Host: pulumi.String("docker.pkg.github.com"),
			RegistryAuth: docker.ProviderRegistryAuthArray{
				docker.ProviderRegistryAuthArgs{
					Address:  pulumi.String("docker.pkg.github.com"),
					Username: &githubActor,
					Password: &githubToken,
				},
			},
		})

		name := "docker.pkg.github.com/Hello-Pulumi/hello-pulumi:latest"
		ubuntuRegistryImage, err := docker.LookupRegistryImage(ctx, &docker.LookupRegistryImageArgs{
			Name: &name,
		})
		if err != nil {
			return err
		}

		image, err := docker.NewRemoteImage(ctx, "remote-image", &docker.RemoteImageArgs{
			Name: pulumi.String(*ubuntuRegistryImage.Name),
			PullTriggers: pulumi.StringArray{
				pulumi.String(ubuntuRegistryImage.Sha256Digest),
			},
		})
		if err != nil {
			return err
		}

		containerDef := image.Name.ApplyString(func(name string) (string, error) {
			fmtstr := `[{
				"name": "my-app",
				"image": %q,
				"portMappings": [{
					"containerPort": 80,
					"hostPort": 80,
					"protocol": "tcp"
				}]
			}]`
			return fmt.Sprintf(fmtstr, name), nil
		})

		// Spin up a load balanced service running NGINX.
		appTask, err := ecs.NewTaskDefinition(ctx, "app-task", &ecs.TaskDefinitionArgs{
			Family:                  pulumi.String("fargate-task-definition"),
			Cpu:                     pulumi.String("256"),
			Memory:                  pulumi.String("512"),
			NetworkMode:             pulumi.String("awsvpc"),
			RequiresCompatibilities: pulumi.StringArray{pulumi.String("FARGATE")},
			ExecutionRoleArn:        taskExecRole.Arn,
			ContainerDefinitions:    containerDef,
		})
		if err != nil {
			return err
		}
		_, err = ecs.NewService(ctx, "app-svc", &ecs.ServiceArgs{
			Cluster:        cluster.Arn,
			DesiredCount:   pulumi.Int(2),
			LaunchType:     pulumi.String("FARGATE"),
			TaskDefinition: appTask.Arn,
			NetworkConfiguration: &ecs.ServiceNetworkConfigurationArgs{
				AssignPublicIp: pulumi.Bool(true),
				Subnets:        toPulumiStringArray(subnet.Ids),
				SecurityGroups: pulumi.StringArray{webSg.ID().ToStringOutput()},
			},
			LoadBalancers: ecs.ServiceLoadBalancerArray{
				ecs.ServiceLoadBalancerArgs{
					TargetGroupArn: webTg.Arn,
					ContainerName:  pulumi.String("my-app"),
					ContainerPort:  pulumi.Int(80),
				},
			},
		}, pulumi.DependsOn([]pulumi.Resource{webListener}))

		// Export the resulting web address.
		ctx.Export("url", webLb.DnsName)
		return nil
	})
}

func toPulumiStringArray(a []string) pulumi.StringArrayInput {
	var res []pulumi.StringInput
	for _, s := range a {
		res = append(res, pulumi.String(s))
	}
	return pulumi.StringArray(res)
}
