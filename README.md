# elasticache-redis-cost

Command elasticache-redis-cost suggests AWS ElastiCache for Redis instance
types that can fit existing Redis instances.

    Usage of elasticache-redis-cost:
      -any-family
        	take into account all instance families, not only memory-optimized
      -any-generation
        	take into account old generation instance types
      -csv
        	print report in CVS instead of formatted text
      -html path
        	path to HTML file to save report; if empty, text report is printed to stdout
      -max-load int
        	source dataset must fit this percent maxmemory utilization of the target, [1,100] range (default 80)
      -redises path
        	path to file with Redis addresses, one per line (/dev/stdin to read from stdin)
      -region region
        	use prices for this AWS region (default "us-east-1")
      -reserved-memory-percent int
        	value of reserved-memory-percent ElastiCache parameter, [0,100] range (default 25)
    
    Please see AWS documentation regarding reserved-memory-percent if you decide to change it:
    
    https://aws.amazon.com/premiumsupport/knowledge-center/available-memory-elasticache-redis-node/
    https://docs.aws.amazon.com/AmazonElastiCache/latest/red-ug/ParameterGroups.Redis.html#ParameterGroups.Redis.3-2-4.New
    
    > The percent of a node's memory reserved for nondata use. By default, the
    > Redis data footprint grows until it consumes all of the node's memory. If
    > this occurs, then node performance will likely suffer due to excessive
    > memory paging. By reserving memory, you can set aside some of the available
    > memory for non-Redis purposes to help reduce the amount of paging.
    
    > This parameter is specific to ElastiCache, and is not part of the standard
    > Redis distribution.

## AWS Environment

This tool uses AWS SDK, please make sure you have AWS credentials available:
<https://docs.aws.amazon.com/cli/latest/userguide/cli-configure-quickstart.html>.

Program only accesses [GetProducts] pricing API endpoint, either use
`AWSPriceListServiceFullAccess` AWS managed policy, or create an explicit
policy:

    {
        "Version": "2012-10-17",
        "Statement": [
            {
                "Action": [
                    "pricing:GetProducts"
                ],
                "Effect": "Allow",
                "Resource": "*"
            }
        ]
    }

[GetProducts]: https://docs.aws.amazon.com/aws-cost-management/latest/APIReference/API_pricing_GetProducts.html
