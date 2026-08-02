# kotozute

これは kotozute のサンプルテンプレートです。以下は生成された内容の簡単な説明です。

```bash
.
├── Makefile                    <-- ビルドを自動化するMake
├── README.md                   <-- この説明ファイル
├── hello-world                 <-- Lambda関数のソースコード
│   ├── main.go                 <-- Lambda関数のコード
│   └── main_test.go            <-- ユニットテスト
└── template.yaml
```

## 必要条件

* 管理者権限で設定済みのAWS CLI
* [Dockerのインストール](https://www.docker.com/community-edition)
* [Golang](https://golang.org)
* SAM CLI - [SAM CLIのインストール](https://docs.aws.amazon.com/serverless-application-model/latest/developerguide/serverless-sam-cli-install.html)

## セットアップ手順

### 依存関係のインストールとターゲットのビルド

この例では、組み込みの `sam build` を使用して依存関係を自動的にダウンロードし、ビルドターゲットをパッケージ化します。
[SAM Buildについて詳しくはこちら](https://docs.aws.amazon.com/serverless-application-model/latest/developerguide/sam-cli-command-reference-sam-build.html)

`sam build` コマンドは `Makefile` の中にラップされています。実行するには以下を実行するだけです。

```shell
make
```

### ローカル開発

**ローカルAPI Gatewayを通じて関数をローカルで呼び出す**

```bash
sam local start-api
```

前のコマンドが正常に実行されれば、次のローカルエンドポイントにアクセスして関数を呼び出せるようになります: `http://localhost:3000/hello`

**SAM CLI** はLambdaとAPI Gatewayの両方をローカルでエミュレートするために使用され、この環境をブートストラップする方法（ランタイム、ソースコードの場所など）を理解するために `template.yaml` を使用します。以下はCLIがAPIとそのルートを初期化するために読み込む内容の抜粋です。

```yaml
...
Events:
    HelloWorld:
        Type: Api # API Event Sourceについての詳細情報: https://github.com/awslabs/serverless-application-model/blob/master/versions/2016-10-31.md#api
        Properties:
            Path: /hello
            Method: get
```

## パッケージ化とデプロイ

AWS LambdaのGolangランタイムでは、ビルドステップで生成された実行可能ファイルを含むフラットなフォルダが必要です。SAMは `CodeUri` プロパティを使用してアプリケーションの場所を把握します。

```yaml
...
    FirstFunction:
        Type: AWS::Serverless::Function
        Properties:
            CodeUri: hello_world/
            ...
```

アプリケーションを初めてデプロイするには、シェルで以下を実行します。

```bash
sam deploy --guided
```

このコマンドはアプリケーションをパッケージ化してAWSにデプロイし、その際に一連のプロンプトが表示されます。

* **Stack Name**: CloudFormationにデプロイするスタックの名前です。アカウントとリージョン内で一意である必要があり、プロジェクト名に合わせたものが良い出発点になります。
* **AWS Region**: アプリケーションをデプロイしたいAWSリージョンです。
* **Confirm changes before deploy**: 「yes」に設定すると、実行前に変更セットが表示され手動でレビューできます。「no」に設定すると、AWS SAM CLIはアプリケーションの変更を自動的にデプロイします。
* **Allow SAM CLI IAM role creation**: この例を含む多くのAWS SAMテンプレートは、含まれるAWS Lambda関数がAWSサービスにアクセスするために必要なAWS IAMロールを作成します。デフォルトでは、これらは必要最小限の権限に制限されています。IAMロールを作成または変更するAWS CloudFormationスタックをデプロイするには、`capabilities` に `CAPABILITY_IAM` の値を指定する必要があります。このプロンプトで許可が与えられない場合、この例をデプロイするには `sam deploy` コマンドに明示的に `--capabilities CAPABILITY_IAM` を渡す必要があります。
* **Save arguments to samconfig.toml**: 「yes」に設定すると、選択内容がプロジェクト内の設定ファイルに保存され、今後はパラメータなしで `sam deploy` を再実行するだけでアプリケーションの変更をデプロイできるようになります。

デプロイ後に表示される出力値の中に、API GatewayエンドポイントのURLを確認できます。

### テスト

Golangに組み込まれている `testing` パッケージを使用しており、以下のコマンドを実行するだけでテストを実行できます。

```shell
cd ./hello-world/
go test -v .
```
# 付録

### Golangのインストール

Go 1.x（'x'は最新バージョン）が、公式のgolangウェブサイトの手順に従ってインストールされていることを確認してください: https://golang.org/doc/install

簡単な方法としては、Homebrew、chocolatey、またはお使いのLinuxパッケージマネージャーを使用することです。

#### Homebrew (Mac)

ターミナルから以下のコマンドを実行します。

```shell
brew install golang
```

すでにインストールされている場合は、以下のコマンドを実行して最新バージョンであることを確認してください。

```shell
brew update
brew upgrade golang
```

#### Chocolatey (Windows)

powershellから以下のコマンドを実行します。

```shell
choco install golang
```

すでにインストールされている場合は、以下のコマンドを実行して最新バージョンであることを確認してください。

```shell
choco upgrade golang
```

## 次のレベルへ

このプロセス全体の理解を深めるために使える、いくつかのアイデアを紹介します。

* 追加のAPIリソース（例: /hello/{proxy+}）を作成し、この新しいパス経由でリクエストされた名前を返す
* それを検証するためにユニットテストを更新する
* パッケージ化とデプロイ

次に、hello worldサンプルを超えて、他の人がサーバーレスアプリケーションをどのように構成しているかについて詳しく知るために、以下のリソースを利用できます。

* [AWS Serverless Application Repository](https://aws.amazon.com/serverless/serverlessrepo/)
