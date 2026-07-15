# MaritacaProxy

Proxy API local compatível com OpenAI que roteia requisições para os modelos do **Maritaca AI (chat.maritaca.ai)** com suporte completo a tool calls, streaming, multi-contas com rotação, e criação automática de contas via e-mail temporário.

[![Go](https://img.shields.io/badge/Go-1.23-00ADD8)](https://go.dev/)
[![License: ISC](https://img.shields.io/badge/License-ISC-yellow.svg)](LICENSE)

---

## Features

- **OpenAI API Compatible** — Interface compatível com `/v1/chat/completions`, `/v1/models`, `/v1/accounts`.
- **Tool Execution** — Sistema completo de execução de ferramentas locais integrado ao fluxo do chat (formato `<tool_call>` com parser streaming idêntico ao qwenProxy).
- **Multi-Account** — Gerencie múltiplas contas Maritaca com rotação round-robin e cooldown automático.
- **Auto Account Creation** — Criação automática de contas usando e-mail temporário (mail.tm, guerrillamail, 1secmail) com verificação de email via Playwright.
- **Streaming** — Suporte completo a streaming SSE no formato OpenAI.
- **Reasoning Support** — Suporte ao modo de pensamento (reasoning) dos modelos Sabia.
- **Token Refresh** — Renovação automática de access tokens via refresh tokens.
- **AES-256-GCM Encryption** — Senhas e tokens são criptografados em repouso.
- **CLI Binary** — Use o comando `maritacaproxy` diretamente.

---

## Arquitetura

```
┌──────────────────┐       ┌──────────────────────┐
│  OpenAI SDK      │       │  MaritacaProxy (Go)  │
│  / cURL / etc    │──────▶│  HTTP API Server     │
└──────────────────┘       │  - /v1/chat/completions
                           │  - /v1/models
                           │  - /v1/accounts
                           │  - Tool Parser (<tool_call>)
                           │  - Stream Handler (SSE)
                           └────┬────────────┬─────┘
                                │            │
                  ┌─────────────▼──┐  ┌──────▼──────┐
                  │ Account Manager│  │  AutoCreate │
                  │ (AES-256-GCM)  │  │  - tempmail │
                  │ Round-robin    │  │  - Auth0    │
                  │ Cooldowns      │  │  - Playwright│
                  └────────────────┘  └─────────────┘
                                │
                  ┌─────────────▼──────────────┐
                  │  chat.maritaca.ai (Maritaca) │
                  │  Auth0 (auth.maritaca.ai)    │
                  └──────────────────────────────┘
```

---

## Instalação

### Via Go install

```bash
go install github.com/deivid22srk/maritacaproxy/cmd/maritacaproxy@latest
```

### Via build manual

```bash
git clone https://github.com/deivid22srk/maritacaproxy.git
cd maritacaproxy
go build -o maritacaproxy ./cmd/maritacaproxy
```

### Requisitos para Auto Account Creation

- Python 3.10+ com Playwright instalado:
  ```bash
  pip install playwright
  playwright install chromium
  ```
- Chrome/Chromium (auto-detectado ou via `CHROME_PATH`)

---

## Configuração

> **Não é obrigatório criar `.env`!** Todas as variáveis têm defaults sensatos no `internal/config/config.go`. O binário roda direto com `./maritacaproxy`.
>
> Você só precisa definir variáveis de ambiente quando quiser mudar um default. Existem **3 formas** de fazer isso:
>
> ### Forma 1: inline (recomendado pra testes rápidos)
> ```bash
> AUTO_ACCOUNT_ENABLED=true ./maritacaproxy -create-account -count 1
> TEMPMAIL_PROVIDER=guerrillamail ./maritacaproxy
> ```
>
> ### Forma 2: exportar no shell
> ```bash
> export AUTO_ACCOUNT_ENABLED=true
> export TEMPMAIL_PROVIDER=mailtm
> ./maritacaproxy
> ```
>
> ### Forma 3: arquivo `.env` (recomendado pra deploy)
> Crie `.env` na raiz (veja `.env.example`), mas note que **o binário não carrega `.env` automaticamente** — você precisa sourced-o antes:
> ```bash
> set -a; . ./.env; set +a
> ./maritacaproxy
> ```
> Ou use `dotenv`:
> ```bash
> npm install -g dotenv-cli
> dotenv -- ./maritacaproxy
> ```

### Variáveis suportadas (com defaults)

```env
# Servidor
PORT=3000
HOST=0.0.0.0
API_KEY=                          # opcional - se vazio, auth é desabilitada

# Auth0 (Maritaca usa Auth0 em auth.maritaca.ai)
AUTH0_DOMAIN=auth.maritaca.ai
AUTH0_CLIENT_ID=qBJrntH9D92AA5n0PR0ph1h54hSqP3C6
AUTH0_AUDIENCE=https://chat.maritaca.ai/api
AUTH0_SCOPE=openid profile email offline_access chat:messages
AUTH0_REDIRECT_URI=https://chat.maritaca.ai/auth
AUTH0_CONNECTION=Username-Password-Authentication

# Maritaca
MARITACA_BASE_URL=https://chat.maritaca.ai

# Temporary Email Provider: mailtm | guerrillamail | 1secmail
TEMPMAIL_PROVIDER=mailtm
AUTO_ACCOUNT_HEADLESS=true
CHROME_PATH=                          # auto-detectado se vazio
AUTO_ACCOUNT_PASSWORD=MaritacaProxy@2024
AUTO_VERIFY_INTERVAL=5
AUTO_VERIFY_MAX_ATTEMPTS=60

# Encryption key for stored credentials (32 bytes hex)
# Leave empty to auto-generate and store in data/.enc_key
MARITACA_PROXY_ENCRYPTION_KEY=
```

---

## Uso

### Iniciar o servidor

```bash
./maritacaproxy
```

O servidor inicia em `http://localhost:3000` com as seguintes rotas:

| Rota | Método | Descrição |
|------|--------|-----------|
| `/v1/chat/completions` | POST | Chat completions (streaming + non-streaming) |
| `/v1/chat/completions/stop` | POST | Abortar uma geração ativa |
| `/v1/models` | GET | Listar modelos disponíveis |
| `/v1/models/:model` | GET | Informações de um modelo específico |
| `/v1/accounts` | GET | Listar contas configuradas |
| `/v1/accounts/create` | POST | Criar nova conta automaticamente (body: `{"count": N}`) |
| `/v1/accounts/:id` | DELETE | Remover uma conta |
| `/health` | GET | Health check |

### Criar contas automaticamente (CLI)

```bash
# Criar 3 contas
AUTO_ACCOUNT_ENABLED=true ./maritacaproxy -create-account -count 3
```

### Criar contas automaticamente (HTTP)

```bash
# Iniciar servidor com AUTO_ACCOUNT_ENABLED=true
AUTO_ACCOUNT_ENABLED=true ./maritacaproxy &

# Disparar criação em background
curl -X POST http://localhost:3000/v1/accounts/create \
  -H "Content-Type: application/json" \
  -d '{"count": 2}'
```

### Exemplos de uso da API

#### OpenAI SDK (Python)

```python
from openai import OpenAI

client = OpenAI(
    base_url="http://localhost:3000/v1",
    api_key="sua-chave"  # ou "sk-no-key-required" se API_KEY não estiver configurado
)

# Chat simples
completion = client.chat.completions.create(
    model="sabia-4",
    messages=[{"role": "user", "content": "Olá!"}]
)
print(completion.choices[0].message.content)

# Com tool calls
completion = client.chat.completions.create(
    model="sabia-4",
    messages=[{"role": "user", "content": "Qual o clima em SP?"}],
    tools=[{
        "type": "function",
        "function": {
            "name": "get_weather",
            "description": "Get current weather",
            "parameters": {
                "type": "object",
                "properties": {"city": {"type": "string"}},
                "required": ["city"]
            }
        }
    }]
)
```

#### cURL (streaming)

```bash
curl -N http://localhost:3000/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "sabia-4",
    "messages": [{"role": "user", "content": "Olá!"}],
    "stream": true
  }'
```

---

## Modelos Suportados

- `sabia-3`
- `sabia-4`
- `sabia-4-pro`
- `sabia-4-thinking` (com reasoning)
- `sabiazinho-3`, `sabiazinho-4`, `sabiazinho-4-pro`
- `sabia2-medium`, `sabia2-small`
- Sufixos `-thinking` e `-no-thinking` suportados em todos

---

## Como funciona a criação automática de contas

1. **E-mail temporário**: Um novo endereço é criado via mail.tm (ou outro provider).
2. **Signup no Auth0**: O endpoint `POST /dbconnections/signup` do Auth0 registra o usuário na connection `Username-Password-Authentication`.
3. **Verificação de email**: O email de verificação enviado pela Maritaca é pollado do provedor temporário. A URL de verificação do Auth0 (`https://auth.maritaca.ai/u/email-verification?ticket=...`) é extraída do corpo do email (ignorando links de tracker como `url5648.maritaca.ai`).
4. **Confirmação**: Um browser headless (via Playwright/Chromium) visita a URL de verificação. O Auth0 marca o email como verificado.
5. **OAuth Login**: Outra execução do Playwright realiza o fluxo Authorization Code + PKCE:
   - Navega para `/authorize` com PKCE challenge
   - Preenche email/senha no form universal do Auth0
   - Captura o redirect para `/auth?code=...`
   - Extrai o authorization code
6. **Token Exchange**: O code é trocado por access_token + refresh_token via `POST /oauth/token`.
7. **Storage**: A conta (com tokens criptografados AES-256-GCM) é armazenada em `data/maritacaproxy.db.json`.

---

## Estrutura do Projeto

```
maritacaproxy/
├── cmd/
│   └── maritacaproxy/
│       └── main.go              # Entry point
├── internal/
│   ├── api/
│   │   └── server.go            # HTTP API handlers
│   ├── account/
│   │   └── manager.go           # Account storage, rotation, encryption
│   ├── auth/
│   │   └── auth0.go             # Auth0 OAuth flow
│   ├── autocreate/
│   │   ├── autocreate.go        # Auto account creation orchestrator
│   │   └── wrapper.go           # Public API
│   ├── config/
│   │   └── config.go            # Env-based config
│   ├── logger/
│   │   └── logger.go            # Structured logger
│   ├── maritaca/
│   │   └── client.go            # Maritaca chat API client + SSE parser
│   ├── tempmail/
│   │   └── tempmail.go          # Temporary email providers
│   └── tools/
│       ├── parser.go            # StreamingToolParser (port of qwenproxy)
│       └── contract.go          # Tool contract builder
├── go.mod
├── go.sum
├── .env.example
├── Dockerfile
├── LICENSE
└── README.md
```

---

## Troubleshooting

| Problema | Solução |
|----------|---------|
| `No available accounts` | Crie contas via `-create-account` ou `POST /v1/accounts/create` |
| Chrome não encontrado | Instale via `playwright install chromium` ou set `CHROME_PATH` |
| `AnomalyDetected` no Auth0 | Auth0 bloqueia login programático sem browser. O Playwright resolve isso. |
| Verification email não chega | Verifique conexão, tente outro `TEMPMAIL_PROVIDER` |
| Token expirado | Refresh automático acontece se `refresh_token` foi armazenado |

---

## Disclaimer

> Este projeto é fornecido estritamente para fins educacionais e de pesquisa.
>
> Os autores não incentivam ou endossam:
> - Violação dos Termos de Serviço da plataforma Maritaca AI.
> - Automação não autorizada em larga escala.
> - Uso para atividades maliciosas.
>
> **Use por sua conta e risco.**
