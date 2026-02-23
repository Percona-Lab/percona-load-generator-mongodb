# Percona Load Generator for MongoDB Clusters - Docker & Kubernetes Guide

This guide details how to containerize and run the `plgm` workload generator.

Running the benchmark as a container inside your Kubernetes cluster is the **recommended approach** for performance testing. It bypasses local network proxies (VPNs, Ingress Controllers) and places the load generator on the same high-speed network fabric as the database, ensuring you measure database performance, not network latency.

## 1. Build the Docker Image

We use a multi-stage Dockerfile to build a lightweight Alpine Linux image. This process uses the `Makefile` to automatically inject the version string.

### Create the `Dockerfile`

Create a file named `Dockerfile` in the root of this project. We have provided a full example here: [Dockerfile](./Dockerfile)

### Build & Tag

Build the image locally.

```bash
docker build -t plgm:latest .
```

> **Note for Kubernetes Users:** If your cluster is remote (EKS, GKE, AKS), you might have to tag and push this image to a registry your cluster can access:
> ```bash
> docker tag plgm:latest myregistry.azurecr.io/plgm:v1.0.0
> docker push myregistry.azurecr.io/plgm:v1.0.0
> ```

---

## 2. Run in Kubernetes (Job)

A Kubernetes Job is the ideal choice for benchmarking as it runs to completion and then terminates. However, you may choose the deployment strategy that best fits your specific requirements.

### Create `plgm-job.yaml`

We have provided a comprehensive sample manifest. It uses a Seed List for the URI (listing all three pods) to ensure high availability and utilizes the `PLGM_REPLICA_SET` variable among others to configure our options. This file is provided as an example; please edit [plgm-job.yaml](./plgm-job.yaml) to suit your specific requirements.

### Execute the Benchmark

**1. Launch the Job**
```bash
kubectl apply -f plgm-job.yaml
job.batch/plgm created
```

**2. Watch the Output**
Find the pod created by the job and stream the logs to see the real-time "Ops/Sec" report.
```bash
# Get the pod name (e.g., plgm-xxxxx)
kubectl get pods -l job-name=plgm -n lab
NAME                 READY   STATUS    RESTARTS   AGE
plgm-xfznq   1/1     Running   0          4s

# Stream logs
kubectl logs plgm-xfznq -n lab

  plgm 1
  --------------------------------------------------
  Database:     airline
  Workers:      40 active
  Duration:     10s
  Report Freq:  1s

  WORKLOAD DEFINITION
  --------------------------------------------------
  Batch Size:    1000
  Mode:          Default Workload
  Distribution:  Select (60%)  Update (20%)
                 Insert (10%)  Delete (10%)

  [INFO] Loaded 1 collection definition(s)
  [INFO] Loaded 2 query templates(s)
  [INFO] Skipping sharding for 'flights': Cluster is not sharded (Replica Set)
  [INFO] Created 4 indexes on 'flights'
  [INFO] Skipping data seeding (configured)

> Starting Workload...

 TIME    | TOTAL OPS | SELECT | INSERT | UPDATE | DELETE
 --------------------------------------------------------
 00:01   |     8,300 |  5,004 |    798 |  1,650 |    848
 00:02   |     8,048 |  4,736 |    773 |  1,694 |    845
 00:03   |     8,168 |  4,728 |    824 |  1,737 |    879
 00:04   |     8,182 |  4,893 |    817 |  1,695 |    777
 00:05   |     8,504 |  5,047 |    843 |  1,724 |    890
 00:06   |     8,776 |  5,271 |    851 |  1,757 |    897
 00:07   |     8,546 |  5,145 |    880 |  1,699 |    822
 00:08   |     8,365 |  4,945 |    828 |  1,753 |    839
 00:09   |     8,733 |  5,208 |    907 |  1,716 |    902
 00:10   |     6,084 |  3,718 |    551 |  1,236 |    579

> Workload Finished.

  SUMMARY
  --------------------------------------------------
  Runtime:    10.00s
  Total Ops:  81,746
  Avg Rate:   8,174 ops/sec

  LATENCY DISTRIBUTION (ms)
  --------------------------------------------------
  TYPE             AVG          P95          P99
  ----             ---          ---          ---
  SELECT       1.24 ms      4.00 ms      9.00 ms
  INSERT      12.11 ms     66.00 ms     73.00 ms
  UPDATE       9.71 ms     65.00 ms     71.00 ms
  DELETE       9.60 ms     65.00 ms     72.00 ms
```

**3. Clean Up & Retry**
Jobs are immutable. To run again with new settings, delete the old job first.
```bash
kubectl get jobs -l job-name=plgm -n lab
NAME           STATUS     COMPLETIONS   DURATION   AGE
plgm   Complete   1/1           13s        3m8s

kubectl delete job plgm -n lab
job.batch "plgm" deleted

kubectl apply -f plgm-job.yaml
```

---

## 3. Running PLGM with TLS on Kubernetes

PLGM can also be executed in Kubernetes environments where TLS is enabled. The steps below describe how to run the workload against a MongoDB deployment created with the Percona Operator for MongoDB, after completing the [process to configure TLS using cert-manager](https://docs.percona.com/percona-operator-for-mongodb/tls-cert-manager.html).

The process for building the Docker image remains unchanged ([please refer to the previous section for detailed instructions](#1-build-the-docker-image)).

When using cert-manager, you must modify the Job YAML definition accordingly. Cert-manager creates a Kubernetes TLS secret that stores the public certificate and private key as two separate files: tls.crt and tls.key. However, the MongoDB Go driver’s tlsCertificateKeyFile parameter requires a single combined PEM file containing both the certificate and private key concatenated together.

When the Percona Operator initializes its MongoDB pods, it runs an internal script to merge these two files into /tmp/tls.pem. The same approach must be implemented for the PLGM load generator by using an initContainer to combine the files before the main container starts.

Here are the steps (once the image has been built):

**1. Create the job yaml**

A custom example Job YAML for PLGM with this logic has been provided, please see [plgm-tls-job.yaml](./plgm-tls-job.yaml). Please update the certificate secret names if you have customized them in your environment. Additionally, ensure that the MongoDB URI, credentials, service name (e.g., mongos), and any other environment-specific parameters are adjusted to match your deployment.

**2. Deploy the job**

```bash
kubectl apply -f plgm-tls-job.yaml
job.batch/plgm-benchmark created
```

**3. Validate**

```bash
kubectl get pods -l job-name=plgm-benchmark -n lab
NAME                   READY   STATUS    RESTARTS   AGE
plgm-benchmark-754zn   1/1     Running   0          26s
```

**4. Check the logs**

```bash
kubectl logs -f plgm-benchmark-754zn -n lab -c plgm
  [INFO] Performance: Automatically disabled driver compression (compressors=none)
2026/02/23 14:00:38 PPROF server running on localhost:6060

  plgm dev
  --------------------------------------------------
  Target URI:     mongodb://lab-mongos.lab.svc.cluster.local:27017/?tls=true&tlsInsecure=true&tlsCertificateKeyFile=/etc/ssl/tls.pem&authSource=admin&compressors=none
  Namespaces:     airline.flights
  Workers:        40 active
  Duration:       60s
  Workload Mode:  Default (Only default.json)

  ACTIVE OVERRIDES (Env)
   -> PLGM_CONCURRENCY=40
   -> PLGM_DEFAULT_WORKLOAD=true
   -> PLGM_DIRECT_CONNECTION=false
   -> PLGM_DURATION=60s
   -> PLGM_READ_PREFERENCE=nearest
   -> PLGM_REPLICASET_NAME=
   -> PLGM_URI=mongodb://lab-mongos.lab.svc.cluster.local:27017/?tls=true&tlsInsecure=true&tlsCertificateKeyFile=/etc/ssl/tls.pem&authSource=admin
   -> PLGM_USERNAME=plgmUser

  WORKLOAD DEFINITION
  --------------------------------------------------
  Distribution:  Select (54%)  Update (21%)
                 Insert (5%)   Delete (10%)
                 Agg    (5%)   Trans  (0%)

  [INFO] Collection 'flights' already set up (Sharded, 42 chunks). Skipping creation.
  [INFO] Collection 'flights' already set up. Skipping index creation.

> Starting Workload...

 TIME    | TOTAL OPS |  SELECT |  INSERT |  UPSERT |  UPDATE |  DELETE |    AGG | TRANS
 -----------------------------------------------------------------------------------------
 00:01   |     3,486 |   1,046 |   1,850 |       0 |     396 |     194 |      0 |     0
 00:02   |     4,714 |   1,354 |   2,590 |       0 |     527 |     243 |      0 |     0
 00:03   |     5,517 |   1,667 |   2,930 |       0 |     623 |     297 |      0 |     0
 00:04   |     4,589 |   1,257 |   2,560 |       0 |     523 |     249 |      0 |     0
 00:05   |     4,045 |   1,201 |   2,160 |       0 |     473 |     211 |      0 |     0
 00:06   |     4,407 |   1,320 |   2,330 |       0 |     522 |     235 |      0 |     0
 00:07   |     6,249 |   1,862 |   3,390 |       0 |     658 |     339 |      0 |     0
 00:08   |     4,650 |   1,454 |   2,370 |       0 |     572 |     254 |      0 |     0
 00:09   |     4,855 |   1,438 |   2,610 |       0 |     549 |     258 |      0 |     0
 00:10   |     6,468 |   1,863 |   3,450 |       0 |     784 |     371 |      0 |     0
 00:11   |     5,435 |   1,581 |   2,940 |       0 |     615 |     299 |      0 |     0
 00:12   |     5,442 |   1,562 |   3,020 |       0 |     566 |     294 |      0 |     0
 00:13   |     5,862 |   1,685 |   3,150 |       0 |     706 |     321 |      0 |     0
 00:14   |     5,892 |   1,758 |   3,100 |       0 |     694 |     340 |      0 |     0
 00:15   |     5,392 |   1,555 |   2,930 |       0 |     623 |     284 |      0 |     0
 00:16   |     5,675 |   1,674 |   3,130 |       0 |     590 |     281 |      0 |     0
 00:17   |     5,516 |   1,645 |   2,950 |       0 |     620 |     301 |      0 |     0
 00:18   |     5,769 |   1,737 |   3,010 |       0 |     698 |     324 |      0 |     0
 00:19   |     5,361 |   1,566 |   2,920 |       0 |     582 |     293 |      0 |     0
 00:20   |     6,271 |   1,857 |   3,400 |       0 |     676 |     338 |      0 |     0
 00:21   |     6,366 |   1,825 |   3,450 |       0 |     754 |     337 |      0 |     0
 00:22   |     4,814 |   1,421 |   2,560 |       0 |     547 |     286 |      0 |     0
 00:23   |     5,752 |   1,735 |   3,010 |       0 |     681 |     326 |      0 |     0
 00:24   |     7,085 |   2,019 |   3,890 |       0 |     784 |     392 |      0 |     0
 00:25   |     4,229 |   1,270 |   2,240 |       0 |     476 |     243 |      0 |     0
 00:26   |     5,417 |   1,621 |   2,820 |       0 |     661 |     315 |      0 |     0
 00:27   |     5,741 |   1,573 |   3,260 |       0 |     612 |     296 |      0 |     0
 00:28   |     5,743 |   1,719 |   3,030 |       0 |     653 |     341 |      0 |     0
 00:29   |     6,095 |   1,768 |   3,330 |       0 |     683 |     314 |      0 |     0
 00:30   |     5,657 |   1,636 |   3,090 |       0 |     634 |     297 |      0 |     0
 00:31   |     4,778 |   1,367 |   2,640 |       0 |     543 |     228 |      0 |     0
 00:32   |     5,295 |   1,579 |   2,770 |       0 |     632 |     314 |      0 |     0
 00:33   |     6,298 |   1,803 |   3,480 |       0 |     706 |     309 |      0 |     0
 00:34   |     4,956 |   1,477 |   2,640 |       0 |     562 |     277 |      0 |     0
 00:35   |     4,412 |   1,209 |   2,420 |       0 |     494 |     289 |      0 |     0
 00:36   |     4,759 |   1,398 |   2,560 |       0 |     547 |     254 |      0 |     0
 00:37   |     5,849 |   1,658 |   3,220 |       0 |     660 |     311 |      0 |     0
 00:38   |     4,744 |   1,365 |   2,610 |       0 |     515 |     254 |      0 |     0
 00:39   |     6,030 |   1,814 |   3,250 |       0 |     659 |     307 |      0 |     0
 00:40   |     5,663 |   1,728 |   2,850 |       0 |     737 |     348 |      0 |     0
 00:41   |     5,374 |   1,493 |   3,040 |       0 |     581 |     260 |      0 |     0
 00:42   |     4,146 |   1,196 |   2,250 |       0 |     473 |     227 |      0 |     0
 00:43   |     5,897 |   1,789 |   3,090 |       0 |     720 |     298 |      0 |     0
 00:44   |     6,079 |   1,768 |   3,240 |       0 |     725 |     346 |      0 |     0
 00:45   |     4,216 |   1,174 |   2,330 |       0 |     470 |     242 |      0 |     0
 00:46   |     4,799 |   1,433 |   2,630 |       0 |     475 |     261 |      0 |     0
 00:47   |     4,439 |   1,352 |   2,300 |       0 |     515 |     272 |      0 |     0
 00:48   |     5,887 |   1,774 |   3,120 |       0 |     635 |     358 |      0 |     0
 00:49   |     5,708 |   1,704 |   2,970 |       0 |     719 |     315 |      0 |     0
 00:50   |     5,552 |   1,606 |   2,930 |       0 |     678 |     338 |      0 |     0
 00:51   |     4,207 |   1,234 |   2,270 |       0 |     480 |     223 |      0 |     0
 00:52   |     5,321 |   1,563 |   2,840 |       0 |     611 |     307 |      0 |     0
 00:53   |     5,455 |   1,578 |   3,000 |       0 |     582 |     295 |      0 |     0
 00:54   |     4,517 |   1,272 |   2,530 |       0 |     501 |     214 |      0 |     0
 00:55   |     5,184 |   1,566 |   2,690 |       0 |     645 |     283 |      0 |     0
 00:56   |     5,444 |   1,682 |   2,840 |       0 |     627 |     295 |      0 |     0
 00:57   |     5,029 |   1,681 |   2,420 |       0 |     638 |     290 |      0 |     0
 00:58   |     5,304 |   1,592 |   2,810 |       0 |     608 |     294 |      0 |     0
 00:59   |     4,921 |   1,302 |   2,840 |       0 |     519 |     260 |      0 |     0
 01:00   |     5,886 |   1,643 |   3,230 |       0 |     683 |     330 |      0 |     0

> Workload Finished.

  SUMMARY
  --------------------------------------------------
  Runtime:    60.00s
  Total Ops:  318,818
  Avg Rate:   5,313 ops/sec

  LATENCY DISTRIBUTION (ms)
  --------------------------------------------------
  TYPE             AVG          MIN          MAX          P95          P99
  SELECT       6.20 ms      0.17 ms    208.10 ms     42.00 ms     78.00 ms
  INSERT       3.23 ms      0.19 ms     29.17 ms      9.00 ms     15.00 ms
  UPSERT             -            -            -            -            -
  UPDATE      23.48 ms      0.17 ms    290.10 ms     83.00 ms    103.00 ms
  DELETE      23.44 ms      0.17 ms    289.78 ms     84.00 ms    104.00 ms
  AGG                -            -            -            -            -
  TRANS              -            -            -            -            -
```  

---

## 4. Configuration Reference

You can override almost any setting in `config.yaml` using these Environment Variables in your Kubernetes manifest. More variables are accepted, please see our [readme](./README.md#environment-variable-overrides) for a full list:

| Variable | Description |
| :--- | :--- |
| `PLGM_URI` | Connection String (use Internal DNS) |
| `PLGM_CONCURRENCY` | Number of parallel worker threads |
| `PLGM_DURATION` | Test duration (e.g., `60s`, `5m`) |
| `PLGM_FIND_PERCENT` | % of operations that are Reads |
| `PLGM_INSERT_PERCENT` | % of operations that are Inserts |
| `PLGM_UPDATE_PERCENT` | % of operations that are Updates |
| `PLGM_DELETE_PERCENT` | % of operations that are Deletes |
| `PLGM_DOCUMENTS_COUNT` | Initial seed document count (if seeding) |
| `PLGM_DEFAULT_WORKLOAD`| Set to `true` (use built-in flights) or `false` (custom) |

## 5. Troubleshooting Performance

**Throughput is low (Bottleneck Analysis)**

1.  **Check the Database Pods:**
    Is the database actually stressed?
    ```bash
    kubectl top pods -n <mongo-namespace>
    ```
    * **High CPU?** The DB is the bottleneck (Good test!).
    * **Low CPU?** The bottleneck is elsewhere (Network or Client).

2.  **Check the Benchmark Pod:**
    Is the generator hitting its own limits?
    ```bash
    kubectl top pod plgm-xxxxx
    ```
    * **CPU Maxed?** The generator is CPU-bound. Increase `resources.limits.cpu` in the YAML or lower `GOMAXPROCS`.
    * **CPU Low?** It might be network latency waiting for the DB. Increase `PLGM_CONCURRENCY` to create more parallel requests.

3.  **Read Preference:**
    If your Primary node is at 100% but Secondaries are idle, ensure your URI includes `readPreference=nearest` or `secondaryPreferred`.