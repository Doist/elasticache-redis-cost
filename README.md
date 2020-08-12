# elasticache-redis-cost

Command elasticache-redis-cost suggests AWS ElastiCache instance types that can
fit existing Redis instances.

        Usage of elasticache-redis-cost:
        -any-family
                take into account all instance families, not only memory-optimized
        -any-generation
                take into account old generation instance types
        -html path
                path to HTML file to save report; if empty, text-only report is printed to stdout
        -max-load percent
                target this percent memory utilization, [1,100] range (default 100)
        -redises path
                path to file with Redis addresses, one per line (/dev/stdin to read from stdin)
        -region region
                use prices for this AWS region (default "us-east-1")
