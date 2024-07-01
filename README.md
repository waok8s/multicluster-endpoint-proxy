# wao-endpoint-proxy
複数クラスターのWAOより取得したスコアをもとにワークロードの適切配置を行うAPIサーバーのプロキシ

## Description
各クラスタに配置されたWAO-Endpoint-Proxyはメトリクス及びカスタムメトリクスをもとにクラスタのスコア（予測消費電力）を算出する。
APIサーバーのリバースプロキシとして動作し、ユーザーからのワークロード配置要求（Deploymento）に対してすk、スコアをもとに分散配置を行う。

## Getting Started

### 必要要件
- go version v1.21.0+
- docker version 17.03+.
- kubectl version v1.29.0+.
- Kubernetes v1.29.0+ cluster.
- metrics-server v0.6.4+
- wao-core v1.29
- wao-metrics-adapter v1.29

### デプロイ方法

**コンテナイメージのビルド:**

```sh
make docker-build
```

**NOTE:** 実行環境でPull可能なレジストリへの配置を別途行う必要があります。

**インストール用マニフェスト作成:**

```sh
make build-installer
```

**インストール前準備:**

- NodeにRegionラベルやnodetypeラベルを付与する

```sh
kubectl label nodes ノード名 topology.kubernetes.io/region=リージョン名
kubectl label nodes ノード名 nodetype=ノード名
```

- metrics-server、wao-core、wao-metrics-adapterのデプロイ
- サーバ証明書のインストール

```sh
subjectAltName=`openssl x509 -ext subjectAltName -noout -in /etc/kubernetes/pki/apiserver.crt | tail -n +2 | sed 's/IP Address/IP/g' |sed 's/ //g'`
touch wao-endpoint-proxy.crt wao-endpoint-proxy.key
sudo openssl req -x509 -nodes -days 365 -new -CA /etc/kubernetes/pki/ca.crt -CAkey /etc/kubernetes/pki/ca.key -keyout wao-endpoint-proxy.key -out wao-endpoint-proxy.crt -subj "/CN=wao-endpoint-proxy" -addext "subjectAltName=$subjectAltName"
kubectl create secret tls wao-endpoint-proxy-system-tls -n wao-system --cert=wao-endpoint-proxy.crt --key=wao-endpoint-proxy.key -o yaml --dry-run=client | kubectl apply -f -
rm wao-endpoint-proxy.crt wao-endpoint-proxy.key
```

**インストール実行:**

```sh
kubectl apply -f dist/install.yaml
```

### クラスタースコアの確認

```sh
kubectl get clusterscore -n wao-system
```

### 他クラスターの追加

```yaml
apiVersion: waoendpointproxy.bitmedia.co.jp/v1beta1
kind: ClusterScore
metadata:
  name: other-cluster1
  namespace: wao-system
spec:
  endpoint: "https://192.168.10.82:18081"
```

**NOTE:** 他クラスターでもwao-endpoint-proxyが動作している必要があります。（自クラスターのリソースはwao-endpoint-proxyが自動で生成するのでユーザーが設定する必要はありません）

### 外部からのアクセス用のサービスを作成
環境に応じてNodePortやLoadBalancerを作成し外部からのアクセスを可能にします。

- selectorには「app: wao-endpoint-proxy」を指定
- targetPortはinstall.yamlのDeployment: spec.template.spec.containers.envのPORT1、PORT2で確認

### kubeconfigの編集

kubectlなどのクライアントでアクセスするためにkubeconfigを編集します。

~/.kube/config

```yaml
clusters:
- cluster:
    certificate-authority-data: LS0tLS1CRUdJTiBDRVJUSUZJQ0FU....
    server: https://10.10.11.101:6443
  name: cluser1
- cluster: # 上記のような通常使用しているclusterをコピーし、接続先とnameを変更する。
    certificate-authority-data: LS0tLS1CRUdJTiBDRVJUSUZJQ0FU....
    server: https://10.10.11.101:32080
  name: wao-endpoint-proxy

contexts:
- context:
    cluster: cluster1
    user: kubernetes-admin
  name: kubernetes-admin@cluster1
- context: # 上記のような通常使用しているcontextをコピーし、clusterとnameを変更する。
    cluster: wao-endpoint-proxy
    user: kubernetes-admin
  name: wao-endpoint-proxy
```

アクセスする場合には contextにwao-endpoint-proxyを指定します。

```sh
kubectl --context=wao-endpoint-proxy get pod
```

## Custom Resources

### クラスタースコア
**API:**
```
GET /apis/waoendpointproxy.bitmedia.co.jp/v1beta1/namespaces/{namespace}/clusterscores
```

**ボディ:**

- ClusterScoreList

|フィールド名|フィールド型|説明／値|
|---|---|---|
|apiVersion|string|waoendpointproxy.bitmedia.co.jp/v1beta1|
|kind|string|ClusterScoreList|
|metadata|ObjectMeta|標準のMetaオブジェクト|
|items|ClusterScore array|ClusterScoreの配列|

- ClusterScore

|フィールド名|フィールド型|説明／値|
|---|---|---|
|apiVersion|string|waoendpointproxy.bitmedia.co.jp/v1beta1|
|kind|string|ClusterScore|
|metadata|ObjectMeta|標準のMetaオブジェクト|
|spec|ClusterScoreSpec|ClusterScoreSpecオブジェクト|
|status|ClusterScoreStatus|ClusterScoreStatusオブジェクト|

- ClusterScoreSpec

|フィールド名|フィールド型|説明／値|
|---|---|---|
|endpoint|string|proxyリクエスト送信先URL（PORT2側URL）|
|owncluster|bool|自クラスターか否かを示す。ユーザーは設定しません|
|region|string|リージョン名。ユーザーは設定しません|

- ClusterScoreStatus

|フィールド名|フィールド型|説明／値|
|---|---|---|
|score|integer|クラスタースコア|

## Properties
**コンテナー環境変数:**

|環境変数名|説明／値|
|---|---|
|FETCH_INTERVAL|クラスタースコアの更新間隔。（秒）|
|LABEL_KEY|wao-endpoint-proxyがワークロード生成時に付与するラベルのキー名|
|LABEL_VALUE_MY_DOMAIN|wao-endpoint-proxyがワークロード生成時に付与するラベルの値。自ドメイン向けに生成するワークロードにのみ使用。他ドメインの値はリージョン名を付与|
|PORT1|ユーザーからのリクエストを待ち受けるポート番号|
|PORT2|他クラスターのwao-endpoint-proxyからのリクエストを待ち受けるポート番号|

## Proxy Detail

### GET
- ClusterScore<br>
自ドメインにそのままリクエストを送信します。
- 上記以外のリソース<br>
全てのクラスターに送信します。
- レスポンス<br>
レスポンスがリスト系（KindがTableあるいは、itemsが存在）の場合、リストをマージして返します。

### POST, PUT
- Deployment、StatefulSet<br>
クラスタースコアに応じた **spec.replicas** を設定した上で、全てのクラスターにリクエストを送信します。
- Namespace、Service、PersistentVolume、PersistentVolumeClaim、Deployment、StatefulSet<br>
当該リソース及びtempleteにラベルを付与します。
- 上記以外のリソース<br>
自ドメインにそのままリクエストを送信します。
- レスポンス<br>
自ドメインのレスポンスを返却します。

### DELETE
- Namespace、Pod、Service、PersistentVolume、PersistentVolumeClaim、Deployment、StatefulSet<br>
全てのクラスターに同じリクエストを送信します。
- 上記以外のリソース<br>
自ドメインにそのままリクエストを送信します。
- レスポンス<br>
自ドメインのレスポンスを返却します。

### HEAD
- すべてのリソース<br>
自ドメインにそのままリクエストを送信します。
- レスポンス<br>
自ドメインのレスポンスを返却します。

### PATCH
- Deployment、StatefulSet<br>
リクエストに **spec.replicas** が存在した場合はクラスタースコアに応じたreplicasを設定した上で、全てのクラスターにリクエストを送信します。存在しない場合は全てのクラスターに同じリクエストを送信します。
- Service、PersistentVolume、PersistentVolumeClaim<br>
全てのクラスターに同じリクエストを送信します。
- 上記以外のリソース<br>
自ドメインにそのままリクエストを送信します。
- レスポンス<br>
自ドメインのレスポンスを返却します。

## License

Copyright 2024.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.

