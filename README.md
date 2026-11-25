# GMINI-Web-Proxy

![Version](https://img.shields.io/badge/version-v1.2.0-blue)
![UI](https://img.shields.io/badge/Web%20UI-v3.0.9-0ea5e9)
![Build](https://img.shields.io/badge/Build-K55%20(0581e87--k55)-1a7f4b)
![Go](https://img.shields.io/badge/Go-stdlib%20z%C3%A9ro%20d%C3%A9pendance-00ADD8)
![Antigravity](https://img.shields.io/badge/Antigravity-%E2%89%A4%20v2.44%2B-6366f1)
![Plateforme](https://img.shields.io/badge/plateforme-macOS%20%2F%20Linux-lightgrey)
![Licence](https://img.shields.io/badge/licence-MIT-green)

**GMINI-Web-Proxy** est un proxy HTTP local écrit en Go (**bibliothèque standard uniquement, zéro dépendance externe**) qui vous permet d'utiliser **les quotas de votre propre compte Google Gemini Web** — particulièrement utile lorsque vous avez atteint votre limite de quota sur **Antigravity** — tout en exposant des API compatibles **OpenAI**, **Anthropic**, **OpenAI Responses** et **Google native**, avec un tableau de bord local (**GMINI Console-Web-UI v3.0.9** sur `/_ui`).

> **Transparence** : GMINI-Web-Proxy **ne contourne aucune limite** et ne fait rien d'autre qu'utiliser les limites normales de **votre propre compte Google**, via l'interface Gemini Web (`gemini.google.com`), distincte de votre quota Antigravity.

**Compatibilité** : testé et vérifié avec **Antigravity jusqu'à `v2.44+`**, ainsi qu'avec tout client OpenAI, Anthropic ou Google CLI. Son architecture modulaire (catalogue dynamique `otAQ7b` et détection en direct) est conçue pour intégrer facilement les futures versions d'Antigravity et les nouveaux modèles Gemini.

---

## Démarrage rapide — Première utilisation

### Pré-requis

- **Go `1.22+`** (compilation statique hors réseau, zéro dépendance externe)
- **Google Chrome** ou **Chromium**
- **Un compte Google** avec accès à [`gemini.google.com`](https://gemini.google.com)
- **Python 3** *(optionnel, pour les scripts d'export/diagnostic et la Console TUI)*
- **Antigravity IDE** *(ou tout client compatible OpenAI / Anthropic / Google)*

### Étape 1 — Cloner & compiler

```bash
git clone https://github.com/nioyco/GMINI-Web-Proxy.git
cd GMINI-Web-Proxy

# Compilation du binaire statique (droits restreints 0700 recommandés)
go build -o gmini-web-proxy .
chmod 0700 gmini-web-proxy
```

### Étape 2 — Configurer `.env` & la clé API locale

```bash
cp .env.example .env
chmod 0600 .env
```

Ouvrez `.env` et renseignez au minimum :

- `GEMINI_API_KEYS` : une clé locale aléatoire (ex. générée via `openssl rand -hex 24`)
- `GEMINI_LISTEN` : `127.0.0.1:8081` *(écoute locale stricte par défaut)*
- `GEMINI_COOKIE_FILE` : chemin vers votre futur fichier `cookie.txt` (ex. `$HOME/Library/Application Support/go-gemini-web2api/cookie.txt` sur macOS ou `$HOME/.config/gmini/cookie.txt` sur Linux)
- Options recommandées : `GEMINI_NO_CONTENT_SEC=240`, `GEMINI_CANNED_ERROR_RETRY=once`, `GEMINI_INTERNAL_TOOL_ACTION=retry`

### Étape 3 — Connexion au compte Google & cookie

1. Démarrez le proxy (voir Étape 4) puis ouvrez `http://127.0.0.1:8081/_ui`.
2. Dans l'onglet **Session & Santé**, cliquez sur **« Lancer la connexion Google »** pour vous connecter dans le profil Chrome dédié (ou importez `__Secure-1PSID`, `__Secure-1PSIDTS` et `SAPISID` sur une seule ligne dans `cookie.txt` en `chmod 0600`).
3. Activez l'**actualisation automatique** du cookie (`on_demand`) ou utilisez **« Actualiser le cookie maintenant »** selon vos besoins.

> **Astuce recommandée** : sur [`gemini.google.com/customize/connectors`](https://gemini.google.com/customize/connectors), désactivez les connecteurs tiers / Workspace inutiles afin d'éviter que Gemini ne déclenche des outils internes Google à la place de vos outils locaux.

### Étape 4 — Démarrage du proxy & de la Console Web UI (`/_ui`)

```bash
./gmini-web-proxy -config .env -listen 127.0.0.1:8081
```

- Vérifiez la santé JSON locale sur `/_status` (avec votre clé API locale) ou ouvrez **`http://127.0.0.1:8081/_ui`**.
- Explorez les 5 onglets de la **Console Web UI `v3.0.9`** :
  - **Session & Santé** : état Google, âge du jeton, pause quota, garde-fous `.env` en direct.
  - **Demandes & Appels** : vue unifiée dédoublonnée par `request_id` avec déroulé par tentative (`OK`, `récupérée`, `échec`).
  - **Diagnostic Proxy** : journal unifié (activités + incidents), filtre par nature, colonne `Requête` / `Récupéré` et export anonymisé (`.zip` / `.csv` / `.md`).
  - **Base outils** : statistiques et historique des appels d'outils locaux et internes (`gemini_tools_db.jsonl`).
  - **Modèles & Antigravity** : catalogue réel détecté chez Google et correspondance avec Antigravity.

### Étape 5 — Brancher Antigravity (ou un client OpenAI/Anthropic)

- **Base URL** : `http://127.0.0.1:8081/v1`
- **Clé API** : votre `GEMINI_API_KEYS` locale
- **Synchronisation des modèles** : sélectionnez les modèles issus de `GET /v1/models` (ou synchronisez `custom_models.json` depuis l'onglet **Modèles & Antigravity**).
- **Pause quota automatique** : si votre compte Google atteint une limite ou un repli modèle, GMINI met automatiquement en pause et protège votre session sans marteler Google.

---

## Sommaire détaillé

1. [Versions & feuille de route (futures versions)](#1-versions--feuille-de-route-futures-versions)
2. [Fonctionnement général & architecture technique](#2-fonctionnement-général--architecture-technique)
3. [Installation avancée & démarrage](#3-installation-avancée--démarrage)
4. [Endpoints HTTP exposés](#4-endpoints-http-exposés)
5. [Configuration complète (`.env` et variables d'environnement)](#5-configuration-complète-env-et-variables-denvironnement)
6. [Checklist de vérification sur GMINI Control (`/_ui`)](#6-checklist-de-vérification-sur-gmini-control-_ui)
7. [Catalogue des messages du proxy dans le chat](#7-catalogue-des-messages-du-proxy-dans-le-chat)
8. [Emplacements des fichiers & commandes de supervision](#8-emplacements-des-fichiers--commandes-de-supervision)
9. [Clients compatibles (Antigravity, OpenAI, Claude Code, Gemini CLI, curl)](#9-clients-compatibles)
10. [Correction des pannes (cas principaux)](#10-correction-des-pannes-cas-principaux)
11. [Sécurité & confidentialité](#11-sécurité--confidentialité)
12. [Contribuer](#12-contribuer)
13. [Crédits & origine du projet](#13-crédits--origine-du-projet)

---

## 1. Versions & feuille de route (futures versions)

Le projet est activement durci autour d'un usage quotidien intensif avec des clients agentiques à outils (Antigravity, Claude Code, Codex CLI, Gemini CLI). Voir [`CHANGELOG.md`](CHANGELOG.md) et [`docs/GUIDE_COMPLET_EXPLOITATION_GMINI.md`](docs/GUIDE_COMPLET_EXPLOITATION_GMINI.md).

| Version | Jalons | Compatibilité Antigravity | Ce qui est livré et vérifié |
|---|---|---|---|
| **`v1.2.0`** *(actuelle · `UI 3.0.9` · Build `K55`)* | **Lots K20 – K55** (base `0581e87`) | **Jusqu'à `v2.44+`** *(vérifié en production > 500 s)* | Web UI `v3.0.9` (vues unifiées `#demandes-unified` dédoublonnée par `request_id` avec déroulé `récupérée / échec` par tentative, `#diag-unified` avec colonne `Requête` + `Récupéré` sur toutes les lignes, export sticky), persistance `CallRecord.BinaryTag`, récupérations post-`internal_tool_retry` (`canned_error`, `empty_reply`, `tool_refusal`), pause quota automatique, base outils `gemini_tools_db.jsonl`, garde-fous `.env` live (`#nc-toggle`, `#cer-toggle`), sync auto `4 Argon`, `code_ref_fastpath`, audit outils K19/K20. |
| **`v1.1.0`** | **Lots K14 – K19** | `v2.31.0` – `v2.32.1` | Sobriété anti-robot K14 (`IdleQuiet=10 min`, backoff `/sorry` `5 → 60 min`, persistance `.state.json`), détection `inner[31]=true` (K15) et réponse vide (K16), `code_ref_fastpath` (K18 T1), auto-refresh cookie `on_demand` (K18 T3/T4), hot-reload `.env` (K18 T5), anti-simulation dans les pensées (K19). |
| **`v1.0.0`** | **Lots P6 – K13** | `v2.30.0` – `v2.31.2` | Écoute locale `127.0.0.1`, clé API obligatoire, arrêt propre `SIGINT`/`SIGTERM`, anti-SSRF, tableau de bord `/_ui`, identité réelle `inner[39]`/`inner[42]`, keepalives SSE `15 s`, `early_stop` (`256 Ko`/`120 s`). |
| **`v0.1.0`** | Base initiale | Clients génériques | Passerelle Go zéro-dépendance issue de `n0madic/go-gemini-web2api`. |

### Intégrer de futures versions d'Antigravity et de nouveaux modèles Gemini

- **Nouveaux modèles Gemini** : le catalogue est interrogé dynamiquement via `otAQ7b` (`pkg/gemini/models.go`) et chaque réponse `StreamGenerate` confirme le couple `(inner[39], inner[42])`. Ainsi, un nouveau modèle ou un renommage côté Google est pris en charge automatiquement sans recompilation, et les éventuelles tables de secours (`modelCatalog` / `GEMINI_THINKING_MODE_CODES`) restent isolées et testées via `GOPROXY=off go test -race -count=1 ./...`.
- **Futures versions d'Antigravity (`> v2.44+`)** : le protocole HTTP local (`/v1/chat/completions`, `/v1/models`, streaming SSE avec `reasoning_content` et `tool_calls`) reste standard. En cas d'évolution de la structure interne d'Antigravity ou de ses délais keepalive, seule la couche d'intégration client est concernée, sans modifier le cœur du proxy.

> **Avertissement technique** : ce proxy s'appuie sur les endpoints web internes de Google Gemini (`StreamGenerate`, `otAQ7b`). Google peut faire évoluer ces formats sans préavis. Le build label `GEMINI_BL` est résolu automatiquement depuis `/app` et conservé dans `.state.json` (24 h par défaut).

---

## 2. Fonctionnement général & architecture technique

### Chaîne complète d'une requête

```text
Client (Antigravity ≤ v2.44+, Claude Code, OpenAI SDK, Gemini CLI…)
   │  HTTP local (ex. POST http://127.0.0.1:8081/v1/chat/completions, SSE)
   ▼
GMINI-Web-Proxy v1.2.0 / UI 3.0.9 (127.0.0.1:8081 uniquement par défaut)
   ├─ 1. Authentifie la clé API locale (GEMINI_API_KEYS) & lit les réglages (.env)
   ├─ 2. Si auto-refresh actif et jeton > 90 min : lance la commande cookie en on_demand (~7 s)
   ├─ 3. Traduit les messages, images & outils → payload web StreamGenerate
   │     (injecte règles d'outils, anti-écho, anti-bac-à-sable interne, inner[45]=1 si temp chat)
   ├─ 4. Ouvre le flux SSE client (keepalives 15 s + note d'envoi à 2 s + notes d'attente 15 s)
   ├─ 5. Appelle gemini.google.com avec cookie SAPISIDHASH + en-têtes modèle (x-goog-ext-525001261-jspb)
   ├─ 6. Analyse les trames wrb.fr en direct :
   │     • Identité réelle du modèle : inner[39] (id hex), inner[42] (nom), inner[31] (erreur interne)
   │     • Synchronise le nom confirmé dans le catalogue (ex. 95a4c0abcba24364 → "4 Argon")
   │     • Pensées (reasoning_content) + texte + blocs ```tool_call```
   │     • Garde-fous mécaniques : anti-écho, early_stop (256 Ko / 120 s), code_ref_fastpath,
   │       séparation read_file+write_file, détection refus/simulation/faux dump + relance auto,
   │       récupérations post-internal_tool_retry, alerte ⚠ GMINI & bilan vérifié des outils
   └─ 7. Persiste les métadonnées sans contenu (0600 : .calls.jsonl, .tokens.jsonl, .flights.json,
         diag, gemini_tools_db.jsonl) et les traces complètes si GEMINI_TRACE_DIR est activé.
```

### Mécanismes clés implémentés

- **Zéro clé API Google payante** : s'appuie sur votre session web Google (`cookie.txt`, schémas `__Secure-1PSID`, `__Secure-1PSIDTS` et `SAPISID` / `SAPISIDHASH`).
- **Sobriété anti-robot (K14)** :
  - Aucun appel de fond vers Google en période de calme (`GEMINI_IDLE_QUIET_MIN=10 min`).
  - La rotation périodique du cookie (`GEMINI_COOKIE_ROTATION`) et la reprise auth (`GEMINI_AUTH_RECOVERY`) sont **désactivées par défaut**.
  - L'actualisation automatique via commande externe (`GEMINI_COOKIE_REFRESH_CMD`) reste en veille au repos et s'exécute automatiquement **juste avant la prochaine demande utilisateur** (`on_demand`) lorsque le jeton dépasse 90 min.
  - En cas de page anti-robot Google (`/sorry`), un backoff exponentiel bloque immédiatement les appels (`5 min → 60 min`) sans marteler Google.
  - L'état de session (`at`, `bl`, catalogue, préférences UI) est persisté dans `*.state.json` (`0600`) pour redémarrer sans recharger `/app`.
- **Catalogue dynamique & synchronisation des noms confirmés (K20)** :
  - Les modèles sont lus sur le compte via `otAQ7b` (et non codés en dur).
  - Dès qu'une vraie réponse `StreamGenerate` confirme un nom dans `inner[42]` pour un `model_id` (`inner[39]`), le proxy **remplace automatiquement** un libellé obsolète renvoyé par `otAQ7b` et le conserve entre redémarrages.
- **Raccourci `code_ref_fastpath` (K18 T1)** :
  - Si Gemini tente malgré tout d'exécuter une commande shell dans son bac à sable interne (```` ```bash?code_reference ````) au lieu d'émettre un `tool_call`, le proxy intercepte la commande simple exploitable dès fermeture du bloc et la convertit immédiatement en appel d'outil `run_command`.
- **Audit mécanique des outils & récupérations post-retry (K19 / K20 / K54)** :
  - Détecte dans le texte **et dans les pensées** les refus fictifs, quotas inventés, rapports simulés et faux dumps collés sans `tool_call`, puis relance automatiquement avec une consigne corrective ciblée.
  - Récupère également les incidents `canned_error`, réponse vide ou `tool_refusal` survenant après `internal_tool_retry`.
  - Sépare automatiquement tout lot prématuré mêlant `read_file` et `write_file` dans le même tour (retient `write_file` pour forcer la lecture préalable).
  - Ajoute en fin de tour dans les pensées un **« Bilan vérifié des outils »** listant les outils réellement transmis par le proxy.
- **Estimation de tokens & persistance incrémentale** :
  - Calcule `input_tokens`, `thought_tokens`, `text_tokens` et `total_tokens` (~1 token / 4 octets + 258 tok/image) attribués au **modèle indiqué par Google**.
  - Persiste sur disque (`0600`) : `*.calls.jsonl`, `*.tokens.jsonl`, `*.flights.json`, `gemini_tools_db.jsonl` et l'anneau de diagnostic.
- **Conversations temporaires à chaud (`inner[45]=1`)** :
  - Activables et désactivables en un clic depuis GMINI Control (`/_ui`) sans redémarrer le proxy ; l'état est conservé dans `.state.json`.

---

## 3. Installation avancée & démarrage

### Compiler et lancer les tests (hors réseau)

```bash
git clone https://github.com/nioyco/GMINI-Web-Proxy.git
cd GMINI-Web-Proxy

# Suite de tests complète hors réseau (avec détecteur de race)
HTTPS_PROXY=http://127.0.0.1:9 HTTP_PROXY=http://127.0.0.1:9 GOPROXY=off go test -race -count=1 ./pkg/apiconv ./pkg/gemini
HTTPS_PROXY=http://127.0.0.1:9 HTTP_PROXY=http://127.0.0.1:9 GOPROXY=off go test -count=1 ./...

# Compilation du binaire statique (droits restreints 0700 recommandés)
go build -o gmini-web-proxy .
chmod 0700 gmini-web-proxy
```

### Préparer la configuration & les cookies manuellement

1. Créer un dossier privé (ex. sur macOS : `"$HOME/Library/Application Support/go-gemini-web2api"`, droits `0700`).
2. Copier `.env.example` vers `.env` (`chmod 0600 .env`) et renseigner au minimum :
   - `GEMINI_API_KEYS=<une-clé-locale-aléatoire>` (ex. `openssl rand -hex 24`)
   - `GEMINI_COOKIE_FILE=<chemin-vers-cookie.txt>`
3. Récupérer vos cookies sur `https://gemini.google.com` (ou via le bouton **Lancer la connexion Google** dans `/_ui`) et les enregistrer sur une seule ligne dans `cookie.txt` (`chmod 0600 cookie.txt`) :
   ```text
   __Secure-1PSID=...; __Secure-1PSIDTS=...; SAPISID=...
   ```
   > **Important** : `__Secure-1PSID`, `__Secure-1PSIDTS` et `SAPISID` sont **tous les trois indispensables**. Sans `__Secure-1PSIDTS`, Google traite silencieusement la session comme anonyme (repli sur le modèle Flash par défaut, pas de vision, pas de catalogue complet).

### Démarrer le proxy

```bash
./gmini-web-proxy \
  -config "$HOME/Library/Application Support/go-gemini-web2api/.env" \
  -cookie-file "$HOME/Library/Application Support/go-gemini-web2api/cookie.txt" \
  -listen 127.0.0.1:8081
```

Ordre de priorité des paramètres :
- `-listen` > `GEMINI_LISTEN` > `127.0.0.1:8081`
- `-config` > `$GEMINI_ENV_FILE` > `./.env`
- `-cookie-file` > `GEMINI_COOKIE_FILE`
- Les variables d'environnement du processus priment toujours sur `.env`.

> **Arrêt propre** : envoyer `SIGINT` (`Ctrl+C`) ou `SIGTERM`. Le proxy laisse jusqu'à `GEMINI_SHUTDOWN_GRACE_SEC` (30 s) aux requêtes en cours pour se terminer proprement.

---

## 4. Endpoints HTTP exposés

| Méthode | Route | Auth API ? | Rôle |
|---|---|---|---|
| `GET` | `/` | Non | Santé basique du service |
| `GET` | `/_status` | Oui | État complet JSON (session, catalogue, blocages, compteurs, appels, tokens, réglages — aucun appel Google) |
| `POST` | `/_models/refresh` | Oui | Rafraîchissement manuel du catalogue via `otAQ7b` (cooldown 5 min) |
| `GET` | `/v1/models` | Non | Catalogue des modèles (format OpenAI par défaut, ou Anthropic si en-tête `anthropic-version`) |
| `GET` | `/v1beta/models` | Non | Catalogue des modèles au format Google natif |
| `POST` | `/v1/chat/completions` | Oui | API OpenAI Chat Completions (streaming SSE, pensées `reasoning_content`, outils, vision) |
| `POST` | `/v1/responses` | Oui | API OpenAI Responses (Codex CLI, streaming + outils) |
| `POST` | `/v1/messages` | Oui | API Anthropic Messages (Claude Code / SDK Anthropic, streaming + outils + vision) |
| `POST` | `/v1/messages/count_tokens` | Oui | Estimation du nombre de tokens Anthropic |
| `POST` | `/v1beta/models/{model}` | Oui | API Google native (`:generateContent` et `:streamGenerateContent`) |
| `GET` | `/_ui` | Local | Tableau de bord **GMINI Control v3.0.9** (génère un jeton de session `X-GMINI-UI`) |
| `GET` | `/_ui/api/state` | `X-GMINI-UI` | Snapshot temps réel pour l'interface `/_ui` |
| `GET` | `/_ui/api/diagnostics` | `X-GMINI-UI` | Flux unifié activités + incidents (fenêtre, pagination) |
| `GET` | `/_ui/api/diagnostics/export` | `X-GMINI-UI` | Export anonymisé (`.zip`, `.csv`, `.md`) |
| `GET` | `/_ui/api/tools-db` | `X-GMINI-UI` | Historique & statistiques de la base outils (`gemini_tools_db.jsonl`) |
| `POST` | `/_ui/api/models/refresh` | `X-GMINI-UI` | Rafraîchir le catalogue depuis `/_ui` |
| `POST` | `/_ui/api/cookie-refresh/toggle` | `X-GMINI-UI` | Activer/désactiver l'actualisation auto du cookie (persisté) |
| `POST` | `/_ui/api/cookie-refresh/run` | `X-GMINI-UI` | Déclencher une actualisation manuelle du cookie (cooldown 3 min) |
| `POST` | `/_ui/api/temporary-chat/toggle` | `X-GMINI-UI` | Activer/désactiver les conversations temporaires `inner[45]=1` (persisté) |
| `POST` | `/_ui/api/guardrails/set` | `X-GMINI-UI` | Modifier les garde-fous `.env` à chaud (`0600`) |
| `POST` | `/_ui/api/config/reload` | `X-GMINI-UI` | Recharger à chaud les réglages non-secrets depuis `.env` |
| `POST` | `/_ui/api/antigravity/launch` | `X-GMINI-UI` | Lancer une copie Antigravity détectée (macOS) |

---

## 5. Configuration complète (`.env` et variables d'environnement)

Toutes les valeurs ci-dessous correspondent aux **vraies valeurs par défaut** compilées dans `config.go` (`v1.2.0`). Voir aussi [`.env.example`](.env.example).

### Réseau & sécurité

| Variable | Défaut | Description |
|---|---|---|
| `GEMINI_LISTEN` | `127.0.0.1:8081` | Adresse d'écoute. Un port nu (`8081`) ou `:8081` reste lié à `127.0.0.1` par sécurité. Passer explicitement `0.0.0.0:8081` pour exposer (déconseillé). |
| `GEMINI_API_KEYS` | *(vide)* | Liste de clés API acceptées (`Authorization: Bearer`, `x-api-key`, `x-goog-api-key`, `?key=`). **Obligatoire au démarrage** sauf `GEMINI_ALLOW_NO_AUTH=1`. |
| `GEMINI_ALLOW_NO_AUTH` | `false` | Autorise le démarrage sans `GEMINI_API_KEYS` (tests locaux uniquement). |
| `GEMINI_CORS_ORIGINS` | *(vide)* | Liste d'origines CORS autorisées (vide = aucun en-tête CORS). |
| `GEMINI_ALLOWED_HOSTS` | *(vide)* | Liste blanche d'en-têtes `Host` autorisés. |
| `GEMINI_PROXY` | *(vide)* | Proxy HTTP(S) sortant (sinon `HTTP_PROXY`/`HTTPS_PROXY` système). |
| `GEMINI_ALLOW_REMOTE_IMAGES` | `false` | Autorise le téléchargement d'URL d'images distantes (désactivé par défaut, anti-SSRF). |
| `GEMINI_ENV_FILE` | `.env` | Chemin du fichier `.env` (surchargé par `-config`). |

### Session Google & sobriété anti-robot (K14 / K18)

| Variable | Défaut | Description |
|---|---|---|
| `GEMINI_COOKIE_FILE` | *(vide)* | Chemin vers le fichier `cookie.txt` (`0600`, recommandé ; surchargé par `-cookie-file`). |
| `GEMINI_COOKIE` | *(vide)* | Chaîne de cookies inline (déconseillé, préférer `GEMINI_COOKIE_FILE`). |
| `GEMINI_AUTH_USER` | *(vide)* | Index du compte Google (`/u/N`) en multi-comptes. |
| `GEMINI_COOKIE_ROTATION` | `false` | Active/désactive la rotation en arrière-plan de `__Secure-1PSIDTS` (`false` recommandé — K14). |
| `GEMINI_COOKIE_REFRESH_MIN` | `9` | Intervalle (min) si `GEMINI_COOKIE_ROTATION=true`. |
| `GEMINI_COOKIE_REFRESH_CMD` | *(vide)* | Commande externe exécutée pour actualiser le cookie (ex. script Chrome headless). |
| `GEMINI_COOKIE_AUTO_REFRESH_ENABLED` | `false` | Active l'auto-refresh `on_demand` au démarrage (aussi pilotable et persisté via `/_ui`). |
| `GEMINI_COOKIE_AUTO_REFRESH_MIN` | `90` | Seuil d'âge du jeton (min) avant déclenchement `on_demand` avant la requête suivante. |
| `GEMINI_IDLE_QUIET_MIN` | `10` | Fenêtre de calme (min) sans requête utilisateur au-delà de laquelle tout appel de fond Google est interdit (`0` désactive). |
| `GEMINI_AUTH_RECOVERY` | `false` | Surveillance périodique en cas de session non connectée (`false` par défaut — K14). |
| `GEMINI_CATALOG_REFRESH_AFTER_COOKIE` | `false` | Rafraîchit `otAQ7b` après un changement de cookie (`false` par défaut — K14). |
| `GEMINI_MODELS_REFRESH_HOURS` | `0` | Rafraîchissement périodique du catalogue en heures (`0` = désactivé). |
| `GEMINI_BLOCK_MAX_BACKOFF_MIN` | `60` | Plafond du backoff exponentiel après détection `/sorry`. |
| `GEMINI_STATE_FILE` | `<cookie>.state.json` | Fichier d'état persisté (`0600`). |
| `GEMINI_STATE_MAX_AGE_MIN` | `1440` | Durée max (24 h) de réutilisation de `.state.json` au démarrage sans recharger `/app`. |

### Modèles, réflexion & confidentialité

| Variable | Défaut | Description |
|---|---|---|
| `GEMINI_DEFAULT_MODEL` | `gemini-3.5-flash` | Modèle utilisé si aucun modèle n'est précisé ou en mode fallback. |
| `GEMINI_BL` | `auto` | Build label Gemini web (`auto` = résolu depuis `/app` et mis en cache). |
| `GEMINI_THINKING_LEVEL` | *(vide)* | Niveau de réflexion par défaut (`low`, `medium`, `high` ; vide = ne rien forcer). |
| `GEMINI_THINKING_MODE_CODES` | *(table catalogue)* | Correspondance `model_id=mode` en secours d'un ancien `.state.json`. |
| `GEMINI_TEMPORARY_CHAT` | `false` | Active `inner[45]=1` (n'enregistre pas dans l'historique web Gemini ; basculable à chaud dans `/_ui`). |
| `GEMINI_RETRY_ATTEMPTS` | `3` | Nombre de tentatives amont sur erreur transitoire. |
| `GEMINI_RETRY_DELAY_SEC` | `2` | Délai initial entre tentatives (secondes). |
| `GEMINI_REQUEST_TIMEOUT_SEC` | `180` | Timeout par requête amont (secondes). |
| `GEMINI_MAX_PROMPT_BYTES` | `0` | Limite dure sur la taille du prompt (`0` = illimité, sinon HTTP `413`). |
| `GEMINI_PROMPT_WARN_KB` | `300` | Seuil (Ko) d'affichage en orange dans `/_ui`. |

### Streaming, outils & garde-fous mécaniques (K9 – K55)

| Variable | Défaut | Description |
|---|---|---|
| `GEMINI_SSE_KEEPALIVE_SEC` | `15` | Fréquence des battements SSE `: keepalive` (secondes). |
| `GEMINI_WAIT_NOTE_SEC` | `15` | Intervalle des notes d'attente dans `reasoning_content` (`0` = désactivé). |
| `GEMINI_SILENCE_WARN_SEC` | `45` | Seuil d'alerte log si Google reste muet après les pensées. |
| `GEMINI_NO_CONTENT_SEC` | `0` | Timeout si aucun contenu n'arrive (`0` = désactivé ; `240` recommandé dans `.env.example` ; pilotable à chaud dans `/_ui` `#nc-toggle`, écriture `.env` `0600`). |
| `GEMINI_SHUTDOWN_GRACE_SEC` | `30` | Délai de grâce à l'arrêt propre. |
| `GEMINI_STREAM_TOOLS_PROGRESS` | `true` | Diffusion progressive sur le chemin streaming + outils. |
| `GEMINI_STREAM_THOUGHTS` | `true` | Diffusion en direct des pensées (`reasoning_content`). |
| `GEMINI_MAX_HELD_BYTES` | `262144` (256 Ko) | Seuil taille du garde-fou anti-emballement `early_stop`. |
| `GEMINI_MAX_HELD_SEC` | `120` | Seuil durée du garde-fou `early_stop` sans `tool_call` complet. |
| `GEMINI_STRIP_HISTORY_ECHO` | `true` | Filtre l'écho parasite de l'historique en début de réponse. |
| `GEMINI_TOOL_PROMPT_NOECHO_RULES` | `true` | Règles anti-écho dans l'enveloppe d'outils. |
| `GEMINI_TOOL_PROMPT_REMINDER` | `true` | Rappel des règles d'outils avant la dernière demande utilisateur. |
| `GEMINI_NEUTRALIZE_HISTORY_MARKERS` | `true` | Neutralise les faux marqueurs d'historique dans le contenu entrant. |
| `GEMINI_DROP_TEXT_AFTER_TOOL_CALL` | `true` | Supprime le texte parasite émis après un `tool_call`. |
| `GEMINI_NO_INTERNAL_CODE_EXEC_RULE` | `true` | Demande à Gemini de ne pas utiliser son bac à sable de code interne (K17 S2). |
| `GEMINI_CODE_REF_FASTPATH` | `true` | Convertit immédiatement un bloc `bash?code_reference` simple en `run_command` (K18 T1). |
| `GEMINI_INTERNAL_TOOL_ACTION` | `notify` | Action sur outil interne détecté : `notify` \| `retry` \| `off` (`retry` recommandé). |
| `GEMINI_CANNED_ERROR_RETRY` | `off` | Relance sur erreur interne Google (`inner[31]=true`) : `off` \| `once` (pilotable à chaud via `/_ui` `#cer-toggle`, écriture `.env` `0600`). |
| `GEMINI_CANNED_ERROR_RETRY_SEC` | `55` | Temps max écoulé autorisant la relance `canned_error`. |
| `GEMINI_TOOL_AUDIT_NOTE` | `true` | Injecte le bilan vérifié des outils dans les pensées + contrôle anti-mensonge (K20). |
| `GEMINI_SPLIT_READ_WRITE_BATCH` | `true` | Retient `write_file` lorsqu'il est mêlé à `read_file` dans le même lot (K20). |
| `GEMINI_WARN_SUSPECT_FALLBACK` | `true` | Signale dans les pensées un repli probable sur un autre modèle. |
| `GEMINI_WARN_SUSPECT_COOLDOWN_MIN` | `10` | Cooldown (min) entre deux avertissements de repli suspect. |

### Interface `/_ui`, journaux & persistance

| Variable | Défaut | Description |
|---|---|---|
| `GEMINI_UI` | `true` | Active GMINI Control sur `/_ui`. |
| `GEMINI_ANTIGRAVITY_MODELS_FILE` | *(auto macOS)* | Chemin de `custom_models.json` (`off` pour masquer la section). |
| `GEMINI_LOG_REQUESTS` | `false` | Journalise chaque requête entrante au niveau `INFO`. |
| `GEMINI_LOG_FORMAT` | `text` | Format des logs : `text` ou `json`. |
| `GEMINI_CALLS_FILE` | `<state>.calls.jsonl` | Historique des appels 12 h (métadonnées uniquement, `0600`). |
| `GEMINI_TOKENS_FILE` | `<state>.tokens.jsonl` | Historique continu des tokens par modèle Google (`0600`). |
| `GEMINI_FLIGHTS_FILE` | `<state>.flights.json` | Persistance de l'historique des demandes (`0600`). |
| `GEMINI_TOOLS_DB_FILE` | `~/Library/Logs/gmini/gemini_tools_db.jsonl` | Base anonymisée des appels d'outils (`0600`). |
| `GEMINI_TRACE_DIR` | *(vide)* | Dossier de traces complètes opt-in (`0600`, contient prompts/réponses, jamais de cookies). |
| `GEMINI_TRACE_MAX_FILES` | `0` | Nombre max de fichiers de traces conservés (`0` = valeur interne). |
| `GEMINI_TRACE_RAW` | `false` | Enregistre aussi les trames `wrb.fr` brutes dans les traces. |

---

## 6. Checklist de vérification sur GMINI Control (`/_ui`)

Avant de lancer un lot important dans Antigravity, ouvrez **`http://127.0.0.1:8081/_ui`** et vérifiez :

1. **Bandeau supérieur** : aucune bannière rouge (*Proxy injoignable* / *Le proxy a redémarré*) ni orange (*Identité non garantie* / *.env modifié*). Si `.env` a changé, cliquez sur **« Recharger .env »**.
2. **Onglet « Session & Santé »** :
   - Badge vert **`Connectée`** (jamais `Non connectée` ni `Session affaiblie`).
   - **Âge du jeton** `< 90 min`, ou **Actualisation auto activée**.
   - **Catalogue** actif et **Blocage anti-robot : `Aucun`**.
3. **Onglet « Demandes & Appels »** :
   - Chaque requête apparaît une seule fois (dédoublonnée par `request_id`) avec le déroulé des tentatives (`Direct`, `Récupérée (N tent.)`, `Échec`).
4. **Onglet « Diagnostic Proxy »** :
   - Filtrage par nature (`Tous`, `Activités`, `Incidents`, `Erreurs`), colonne `Requête` (`request_id`) et colonne `Récupéré` (`direct`, `récupérée (N)`, `oui`, `non`).
5. **Onglet « Modèles & Antigravity »** :
   - Les modèles détectés affichent leurs noms confirmés (`Indiqué par Google`) et la correspondance avec Antigravity est à jour.

---

## 7. Catalogue des messages du proxy dans le chat

Le proxy distingue :
- **Notes techniques dans les pensées (`reasoning_content`)**, préfixées `**[HH:MM:SS] GMINI — …**` ou `GMINI (pas le modèle — attente technique)` : informent en temps réel sans polluer la réponse finale.
- **Alertes dans la réponse finale** : uniquement en cas de panne bloquante ou d'anomalie mécanique vérifiée.

| Message visible | Emplacement | Signification & conduite à tenir |
|---|---|---|
| `GMINI : actualisation de la session…` | Pensées | Le jeton dépassait 90 min : rafraîchissement silencieux `on_demand` (~7 s) avant l'envoi. **Normal.** |
| `Demande envoyée à <modèle> (<N> Ko), en attente de Google…` | Pensées (à `2 s`) | La requête est partie chez Google. **Normal.** |
| `Google n'a encore envoyé aucune pensée ni réponse (N s d'attente ; K battement(s))` | Pensées (toutes les `15 s`) | Google réfléchit / file d'attente ; le flux reste maintenu vivant par les keepalives. **Normal** (jusqu'à ~60–120 s sur gros prompt). |
| `Google : <statut>` | Pensées | Statut intermédiaire renvoyé par Google (`body[2]["7"]`). |
| `Préparation d'un appel d'outil… N Ko reçus` / `appel d'outil reçu, Gemini continue d'écrire` | Pensées | Gros `tool_call` (ex. `write_file`) en cours de réception avant émission structurée. **Normal.** |
| `Gemini lance un outil interne ; la réponse peut tarder` / `outil interne d'exécution de code` | Pensées | Google tente un outil interne ou `code_reference`. Avec `code_ref_fastpath=on`, une commande shell simple est convertie aussitôt en `run_command`. |
| `Écriture prématurée retenue` | Pensées (K20) | Le modèle a envoyé `read_file` + `write_file` dans le même lot : `write_file` est retenu pour imposer la lecture préalable. **Automatique.** |
| `Bilan vérifié des outils : Depuis votre dernière demande : …` | Pensées (fin de tour, K20) | Preuve mécanique infalsifiable des outils réellement transmis par le proxy depuis votre dernier message. |
| `GMINI : jeton de N min — risque de repli sur un autre modèle` | Pensées (auto-refresh `off`, > 120 min) | Actualiser le cookie via GMINI Control. |
| `⚠ Note GMINI : Google a répondu avec un autre modèle (<nom>)` | Pensées | Repli silencieux côté Google (cookie âgé ou quota atteint). Actualiser le cookie ou vérifier `gemini.google.com/usage`. |
| `Gemini affirme ne pas avoir d'outils locaux / avoir agi sans outil : nouvelle tentative…` | Pensées (K19/K20) | Refus/simulation ou faux dump de fichier intercepté ; relance automatique unique avec consigne corrective. |
| `> ⚠ GMINI (vérification mécanique) : le modèle affirme avoir mis à jour ou écrit un fichier…` | **Texte final** (K20) | **ALERTE** : le modèle prétend avoir écrit sur disque ou lancé des tests sans qu'aucun `tool_call` correspondant n'ait transité par le proxy. Vérifier le disque et redemander l'appel d'outil explicite. |
| `GMINI : Google a renvoyé une réponse vide sur <modèle> (prompt de N Ko)` | Texte final (K16) | Flux clos sans candidat ni texte. Compacter la conversation (`/compact`), réessayer ou changer de modèle. |
| `GMINI : Google a renvoyé une erreur interne sur <modèle>` | Texte final (K15) | Erreur interne Google (`inner[31]=true`). Réessayer ou ouvrir une nouvelle conversation. |
| `GMINI : Gemini a refusé ou simulé deux fois sans appeler les outils locaux` | Texte final (K19/K20) | Deux refus/simulations consécutifs. Ouvrir une nouvelle conversation (l'historique long a biaisé le modèle). |
| `GMINI : la réponse de Gemini s'emballe (N Ko sans fin) ; demande interrompue` | Texte final (K9) | Garde-fou `early_stop` (`256 Ko` ou `120 s` sans `tool_call` complet). Réessayer ou réduire le prompt. |

---

## 8. Emplacements des fichiers & commandes de supervision

### Carte complète des fichiers (installation standard macOS)

| Type | Emplacement par défaut | Contient des secrets / texte ? | Rôle |
|---|---|---|---|
| **Journal principal** | `$HOME/Library/Logs/gmini/proxy.log` | Non (métadonnées uniquement) | Démarrages, `request_end`, `served_model`, `code_ref_fastpath`, erreurs, interceptions |
| **Base outils** | `$HOME/Library/Logs/gmini/gemini_tools_db.jsonl` | Non (`0600`, métadonnées) | Statistiques et historique des appels d'outils locaux et internes |
| **Traces complètes** | `$HOME/Library/Logs/gmini/traces/*.json` (`0600`) | **Oui : prompts, pensées, réponses et trames** (jamais de cookies ni clés) | Analyse fine d'un échange, inspection des `tool_call` et des latences |
| **Journal des appels** | `$HOME/Library/Application Support/go-gemini-web2api/cookie.txt.calls.jsonl` | Non (`0600`, tailles/délais/identités/tokens/version) | Alimente « Demandes & Appels » et synchronise les noms confirmés |
| **Journal continu tokens** | `$HOME/Library/Application Support/go-gemini-web2api/cookie.txt.tokens.jsonl` | Non (`0600`, append-only) | Historique cumulé des tokens estimés par modèle indiqué par Google |
| **Historique demandes** | `$HOME/Library/Application Support/go-gemini-web2api/cookie.txt.flights.json` | Non (`0600`, jusqu'à 20 conv. / 500 entrées) | Persiste « Demandes & Appels » entre redémarrages |
| **État de session** | `$HOME/Library/Application Support/go-gemini-web2api/cookie.txt.state.json` | **Oui : jeton `at`** (ne jamais partager) | Évite de recharger `/app` au démarrage, conserve catalogue et bascules UI |
| **Cookie & configuration** | `$HOME/Library/Application Support/go-gemini-web2api/{cookie.txt,.env}` | **Oui : secrets** (`0600`, ne jamais partager) | Session Google et paramètres du proxy |

### Commandes prêtes à copier-coller

```bash
# 1. Suivre le journal du proxy en direct
tail -f "$HOME/Library/Logs/gmini/proxy.log"

# 2. Filtrer les avertissements, erreurs, silences et interceptions récents
grep -E "level=(WARN|ERROR)|unverified_action_claim|read_write_batch_split|tool_call_unconverted|code_ref_fastpath|early_stop|served_model_doubt|upstream_silence|upstream_canned_error|upstream_empty_reply|google_blocked" \
  "$HOME/Library/Logs/gmini/proxy.log" | tail -n 50

# 3. Consulter l'état JSON local du proxy (requiert votre clé locale, aucun appel Google)
curl -s -H "Authorization: Bearer <VOTRE_CLE_API>" http://127.0.0.1:8081/_status | python3 -m json.tool

# 4. Lister le catalogue réellement exposé aux clients (local uniquement)
curl -s http://127.0.0.1:8081/v1/models | python3 -m json.tool

# 5. Générer un export anonymisé de diagnostic (sans secret)
python3 tools/export_gmini_diagnostics.py

# 6. Vérifier le processus actif et le port d'écoute 8081
lsof -nP -iTCP:8081 -sTCP:LISTEN
```

---

## 9. Clients compatibles

### Antigravity (IDE, jusqu'à `v2.44+`)
- Base URL : `http://127.0.0.1:8081/v1` (API compatible OpenAI, streaming SSE + outils).
- Les modèles du catalogue (`/v1/models`) sont déclarés dans `custom_models.json` avec relais keepalive > 300 s.

### OpenAI Python SDK / Cherry Studio / ChatBox

```python
from openai import OpenAI

client = OpenAI(base_url="http://127.0.0.1:8081/v1", api_key="<VOTRE_CLE_API>")
resp = client.chat.completions.create(
    model="gemini-detailed-research",  # ou tout id renvoyé par GET /v1/models
    messages=[{"role": "user", "content": "Explique le protocole SSE en 5 points."}],
)
print(resp.choices[0].message.content)
```

### Claude Code / Clients Anthropic (`/v1/messages`)

```bash
export ANTHROPIC_BASE_URL=http://127.0.0.1:8081
export ANTHROPIC_API_KEY="<VOTRE_CLE_API>"
export ANTHROPIC_MODEL="gemini-detailed-research"
claude
```

### Gemini CLI (`/v1beta/models`)

```bash
export GEMINI_API_KEY="<VOTRE_CLE_API>"
export GOOGLE_GEMINI_BASE_URL=http://127.0.0.1:8081
gemini
```

### `curl`

```bash
curl -N http://127.0.0.1:8081/v1/chat/completions \
  -H "Authorization: Bearer <VOTRE_CLE_API>" \
  -H "Content-Type: application/json" \
  -d '{"model":"gemini-quick-answers","messages":[{"role":"user","content":"Bonjour"}],"stream":true}'
```

---

## 10. Correction des pannes (cas principaux)

| Symptôme / Panne | Cause probable | Résolution pas à pas |
|---|---|---|
| **Session `Non connectée` ou repli silencieux sur un autre modèle (`other_model`)** | Cookie Google expiré ou affaibli (`__Secure-1PSIDTS` ancien) | 1. Dans GMINI Control : cliquer **« Actualiser le cookie maintenant »**.<br>2. Si le rafraîchissement silencieux échoue 2 fois : reconnecter le compte Google via **« Lancer la connexion Google »**.<br>3. Vérifier le badge vert `Connectée`. |
| **Blocage anti-robot (`/sorry` / `google_blocked`)** | Trop d'appels rapprochés ou rafraîchissements en boucle | **Ne pas relancer le proxy en boucle.** Attendre la fin du backoff affichée dans GMINI Control (`5 → 60 min`). Vérifier `GEMINI_COOKIE_ROTATION=false` et `GEMINI_AUTH_RECOVERY=false`. |
| **Coupure systématique au bout de 300 s (5 min)** | L'instance Antigravity ouverte n'a pas le relais keepalive | Fermer Antigravity (`Cmd+Q`), vérifier dans GMINI Control (« Modèles & Antigravity ») que la copie lancée a **Relais anti-coupure : `Oui`**. |
| **GMINI Control affiche `401` / `Le proxy a redémarré`** | Onglet ouvert avant un redémarrage (jeton `X-GMINI-UI` régénéré) | Cliquer sur **« Recharger la page »** (`Cmd+R`). |
| **Port `8081` occupé au démarrage** | Ancien processus proxy encore actif | Vérifier `lsof -nP -iTCP:8081 -sTCP:LISTEN`, arrêter proprement (`kill -INT <pid>`), puis redémarrer. |
| **Modèle muet, `canned_error` (`inner[31]=true`) ou réponse vide** | Prompt devenu trop volumineux (`> 300 Ko`, affiché en orange) ou incident Google ponctuel | 1. Activer `GEMINI_CANNED_ERROR_RETRY=once` et `GEMINI_NO_CONTENT_SEC=240` depuis `/_ui`.<br>2. Compacter la conversation (`/compact`) ou ouvrir une nouvelle conversation.<br>3. Vérifier le quota réel sur `gemini.google.com/usage`. |
| **Le modèle prétend avoir modifié un fichier sans rien changer (ou alerte `⚠ GMINI vérification mécanique`)** | Réponse texte sans `tool_call` | 1. Ne pas valider le résultat.<br>2. Contrôler le « Bilan vérifié des outils » dans les pensées et « Demandes & Appels ».<br>3. Redemander explicitement : *« Appelle `write_file` / `edit_file` sur `<chemin>` maintenant, ne colle pas le contenu dans le chat »*. |
| **Pause longue sur `code_reference` ou outil interne** | Gemini tente son bac à sable interne ou un connecteur tiers | Vérifier `code_ref_fastpath=on`, `no_internal_code_exec_rule=on`, `GEMINI_INTERNAL_TOOL_ACTION=retry`, et désactiver les connecteurs inutiles sur `gemini.google.com/customize/connectors`. |

---

## 11. Sécurité & confidentialité

- **Écoute locale stricte** : le proxy écoute sur `127.0.0.1:8081` par défaut et refuse de démarrer sans `GEMINI_API_KEYS` (sauf `GEMINI_ALLOW_NO_AUTH=1`).
- **Permissions restrictives** : les binaires sont en `0700` et tous les fichiers sensibles ou journaux générés (`.env`, `cookie.txt`, `.state.json`, `.calls.jsonl`, `.tokens.jsonl`, `.flights.json`, `gemini_tools_db.jsonl`, traces) sont écrits en `0600`.
- **Aucun secret dans les journaux** : `proxy.log`, `/_status`, `/_ui`, `.calls.jsonl`, `.tokens.jsonl` et `.flights.json` ne contiennent ni cookies, ni clés API, ni contenu de conversation.
- **Règle d'hygiène Git** : [`.gitignore`](.gitignore) exclut automatiquement `.env`, `*.txt`, `*.state.json`, `*.calls.jsonl`, `*.tokens.jsonl`, `*.flights.json`, `*.bak.*` et `.DS_Store`.

Pour plus de détails, consultez [`SECURITY.md`](SECURITY.md).

---

## 12. Contribuer

Les **contributions, retours d'expérience, rapports de bugs et aides au développement sont les bienvenus** !

- Consultez [`CONTRIBUTING.md`](CONTRIBUTING.md) pour la marche à suivre (`gofmt`, `go vet`, `go test -race ./...`, export anonymisé pour les rapports de bugs).
- Consultez [`SECURITY.md`](SECURITY.md) avant de partager des journaux ou de signaler une vulnérabilité.

---

## 13. Crédits & origine du projet

- Ce dépôt est dérivé et profondément étendu à partir du projet open source **[`n0madic/go-gemini-web2api`](https://github.com/n0madic/go-gemini-web2api)** (licence MIT).
- Inspirations protocolaires initiales : [`Sophomoresty/gemini-web2api`](https://github.com/Sophomoresty/gemini-web2api) et [`HanaokaYuzu/Gemini-API`](https://github.com/HanaokaYuzu/Gemini-API).
- Licence : [`LICENSE`](LICENSE) (MIT).
