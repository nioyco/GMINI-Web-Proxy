# GMINI-Web-Proxy

![Version](https://img.shields.io/badge/version-v1.2.0-blue)
![UI](https://img.shields.io/badge/Web%20UI-v3.0.9-0ea5e9)
![Go](https://img.shields.io/badge/Go-stdlib%20z%C3%A9ro%20d%C3%A9pendance-00ADD8)
![État](https://img.shields.io/badge/%C3%A9tat-K55%20(0581e87--k55)%20op%C3%A9rationnel-1a7f4b)
![Plateforme](https://img.shields.io/badge/plateforme-macOS%20%2F%20Linux-lightgrey)

Proxy HTTP local écrit en Go (**bibliothèque standard uniquement, zéro dépendance externe**) qui expose l'interface web de Google Gemini (`StreamGenerate` / `otAQ7b`) sous forme d'API compatible **OpenAI**, **Anthropic**, **OpenAI Responses** et **Google native**, avec un tableau de bord local (**GMINI Console-Web-UI v3.0.9**, `/_ui`), une sobriété réseau anti-robot, une pause quota automatique sur repli modèle, un suivi des tokens par modèle réel, une base d'outils (`gemini_tools_db.jsonl`) et des garde-fous mécaniques avancés pour les agents à outils (notamment **Antigravity** sur macOS).

> **Version actuelle : `v1.2.0` · `UI 3.0.9` (lots K21–K55, build `go-gemini-web2api-p9-0581e87-k55`)** — voir l'historique complet dans [`CHANGELOG.md`](CHANGELOG.md) et la fiche synthétique d'exploitation dans [`docs/GUIDE_COMPLET_EXPLOITATION_GMINI.md`](docs/GUIDE_COMPLET_EXPLOITATION_GMINI.md).

---

## Sommaire

1. [État du développement & versions](#1-état-du-développement--versions)
2. [Fonctionnement général & architecture technique](#2-fonctionnement-général--architecture-technique)
3. [Installation, compilation & mise en route](#3-installation-compilation--mise-en-route)
4. [Endpoints HTTP exposés](#4-endpoints-http-exposés)
5. [Configuration complète (`.env` et variables d'environnement)](#5-configuration-complète-env-et-variables-denvironnement)
6. [Checklist de vérification sur GMINI Control (`/_ui`)](#6-checklist-de-vérification-sur-gmini-control-_ui)
7. [Catalogue des messages du proxy dans le chat](#7-catalogue-des-messages-du-proxy-dans-le-chat)
8. [Emplacements des fichiers & commandes de supervision](#8-emplacements-des-fichiers--commandes-de-supervision)
9. [Clients compatibles (Antigravity, OpenAI, Claude Code, Gemini CLI, curl)](#9-clients-compatibles)
10. [Correction des pannes (cas principaux)](#10-correction-des-pannes-cas-principaux)
11. [Sécurité & confidentialité](#11-sécurité--confidentialité)
12. [Crédits & origine du projet](#12-crédits--origine-du-projet)

---

## 1. État du développement & versions

Le projet est activement durci autour d'un usage quotidien intensif avec des clients agentiques à outils (Antigravity, Claude Code, Codex CLI, Gemini CLI).

| Version | Jalons | Ce qui est livré et vérifié |
|---|---|---|
| **`v1.2.0`** *(actuelle, `0581e87-k24`)* | **Lots K20 – K24** (sur base `0581e87`) | **K21–K24** : rappel d'outils `T2+` (`hasPriorHistory` + contre-exemple `bash?code_reference`), drainage `200 ms` sur *early stop*, maintien `emitWaitNote` si `GEMINI_NO_CONTENT_SEC > 0`, ordre propre `served_model` < `client_cancelled` < `request_end` (`upstream_ms > 0`, `request_id`, `upstream_heartbeats`), silence `gate=quiet` en veille (`userActivityGen`), sondes silencieuses `X-GMINI-Quiet: 1`, `WaitMsBeforeAsync=10000` par défaut, et garde-fous `.env` persistants (`0o600`) pilotables en live via `POST /_ui/api/guardrails/set` (`#nc-toggle`, `#cer-toggle`) et Console `V2.0.0`. **K20** : sync auto **`4 Argon`**, `GEMINI_TOOL_AUDIT_NOTE`, `GEMINI_SPLIT_READ_WRITE_BATCH`, suivi tokens `.calls.jsonl`/`.tokens.jsonl`/`.flights.json`, bascule *Conversations temporaires*. |
| **`v1.1.0`** | **Lots K14 – K19** | Sobriété anti-robot K14 (zéro appel de fond au repos `IdleQuiet=10 min`, rotation cookie & reprise auth `off` par défaut, backoff `/sorry` `5 → 60 min`, persistance `.state.json`) ; détection erreur interne Google (`inner[31]=true`, K15) et réponse vide (K16) ; règle anti-bac-à-sable interne (K17) ; raccourci **`code_ref_fastpath`** (`bash?code_reference` → `run_command`, K18 T1) ; auto-refresh cookie **`on_demand`** persisté (K18 T3/T4) ; détection & hot-reload `.env` (K18 T5) ; détection quotas/rapports simulés dans les pensées (K19). |
| **`v1.0.0`** | **Lots P6 – K13** | Sécurité par défaut (`127.0.0.1:8081`, clé API obligatoire, arrêt propre `SIGINT`/`SIGTERM`, anti-SSRF) ; tableau de bord local **GMINI Control** (`/_ui`) protégé par jeton de session ; lecture d'identité réelle du modèle (`inner[39]`/`inner[42]`) et alerte repli silencieux ; keepalives SSE `15 s`, diffusion des pensées (`reasoning_content`), notes techniques `2 s`/`15 s`, alerte silence `45 s`, anti-écho, `early_stop` (`256 Ko`/`120 s`), traces complètes `0600`. |
| **`v0.1.0`** | Base initiale | Passerelle Go zéro-dépendance issue de `n0madic/go-gemini-web2api`. |

### Matrice de compatibilité Proxy ↔ Antigravity & Enabler P1/P5a

| Version GMINI-Web-Proxy | Versions Antigravity supportées | Enabler / Relais keepalive (> 300 s) | Panneau de contrôle | Notes techniques |
|---|---|---|---|---|
| **`v1.2.0`** *(actuelle)* | **`v2.31.2`** et **`v2.32.1+`** *(vérifié en production > 500 s)* | **Requis** : Enabler P1/P5a commit `ad28f7e` (`dist/lsDefaults.js` / `baseLsArgs` pour `v2.32.1+`) | **`GMINI-Proxy-Control v2.0.0`** (console BIOS 2 colonnes) | Sync auto `4 Argon`, audit outils K20, bascule *Conversations temporaires*, tokens par modèle Google, fix CSS `/_ui`. |
| **`v1.1.0`** *(Lots K14–K19)* | `v2.31.0` – `v2.31.2` | Enabler P1/P5a (`aece4d6`, layout `args` littéral dans `languageServer.js`) | `GMINI-Proxy-Control v1.1.0` / `v1.2.0` | Compatible `v2.32.1` **uniquement** après mise à jour Enabler `ad28f7e` (sinon `UNSUPPORTED_RUNTIME`). |
| **`v1.0.0`** *(Lots P6–K13)* | `v2.30.0` – `v2.31.2` | Optionnel sur `v2.30.0` (coupure à 300 s sans relais) ; recommandé P1/P5a sur `v2.31.x` | Raccourcis séparés `1..10` | Keepalives SSE 15 s, `/_ui`, identité réelle `inner[39]/inner[42]`. |
| **`v0.1.0`** *(base)* | Clients OpenAI / Anthropic / Gemini CLI génériques | Non géré | Aucun | Sans relais anti-coupure, sans sobriété K14 ni garde-fous outils. |

> **Avertissement technique** : ce proxy s'appuie sur les endpoints web internes de Google Gemini (`StreamGenerate`, `otAQ7b`). Google peut faire évoluer ces formats sans préavis. Le build label `GEMINI_BL` est résolu automatiquement depuis `/app` et conservé dans `.state.json` (24 h par défaut).

---

## 2. Fonctionnement général & architecture technique

### Chaîne complète d'une requête

```text
Client (Antigravity P1/P5a, Claude Code, OpenAI SDK, Gemini CLI…)
   │  HTTP local (ex. POST http://127.0.0.1:8081/v1/chat/completions, SSE)
   ▼
GMINI-Web-Proxy v1.2.0 (127.0.0.1:8081 uniquement par défaut)
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
   │       alerte ⚠ GMINI (vérification mécanique) & bilan vérifié des outils en fin de tour
   └─ 7. Persiste les métadonnées sans contenu (0600 : .calls.jsonl, .tokens.jsonl, .flights.json)
         et les traces complètes si GEMINI_TRACE_DIR est activé.
```

### Mécanismes clés implémentés

- **Zéro clé API Google payante** : s'appuie sur la session web Google (`cookie.txt`, schémas `__Secure-1PSID`, `__Secure-1PSIDTS` et `SAPISID` / `SAPISIDHASH`).
- **Sobriété anti-robot (K14)** :
  - Aucun appel de fond vers Google en période de calme (`GEMINI_IDLE_QUIET_MIN=10 min`).
  - La rotation périodique du cookie (`GEMINI_COOKIE_ROTATION`) et la reprise auth (`GEMINI_AUTH_RECOVERY`) sont **désactivées par défaut**.
  - L'actualisation automatique via commande externe (`GEMINI_COOKIE_REFRESH_CMD`) reste en veille au repos et s'exécute automatiquement **juste avant la prochaine demande utilisateur** (`on_demand`) lorsque le jeton dépasse 90 min.
  - En cas de page anti-robot Google (`/sorry`), un backoff exponentiel bloque immédiatement les appels (`5 min → 60 min`) sans marteler Google.
  - L'état de session (`at`, `bl`, catalogue, préférences UI) est persisté dans `*.state.json` (`0600`) pour redémarrer sans recharger `/app`.
- **Catalogue dynamique & synchronisation des noms confirmés (K20)** :
  - Les modèles sont lus sur le compte via `otAQ7b` (et non codés en dur).
  - Dès qu'une vraie réponse `StreamGenerate` confirme un nom dans `inner[42]` pour un `model_id` (`inner[39]`), le proxy **remplace automatiquement** un libellé obsolète renvoyé par `otAQ7b` (ex. `95a4c0abcba24364` affiché **`4 Argon`** même si `otAQ7b` annonce encore `4 Carbon`). Ce nom confirmé survit aux redémarrages et aux rafraîchissements du catalogue.
- **Raccourci `code_ref_fastpath` (K18 T1)** :
  - Si Gemini tente malgré tout d'exécuter une commande shell dans son bac à sable interne (```` ```bash?code_reference ````) au lieu d'émettre un `tool_call`, le proxy intercepte la commande simple exploitable dès fermeture du bloc et la convertit immédiatement en appel d'outil `run_command` (économie de 20 à 60 s de blocage par tour).
- **Audit mécanique des outils & garde-fous anti-mensonge (K19 / K20)** :
  - Détecte dans le texte **et dans les pensées** les refus fictifs, quotas inventés, rapports simulés et faux dumps de fichiers collés dans le chat sans `tool_call`.
  - Relance automatiquement une fois avec une consigne corrective ciblée ; si l'anomalie persiste, affiche un bandeau rouge/orange explicite `⚠ GMINI (vérification mécanique)` dans la réponse finale.
  - Sépare automatiquement tout lot prématuré mêlant `read_file` et `write_file` dans le même tour (retient `write_file` pour forcer la lecture préalable, sans perturber les lots légitimes `read_file` + `edit_file`).
  - Ajoute en fin de tour dans les pensées un **« Bilan vérifié des outils »** mécanique listant les outils réellement transmis par le proxy depuis le dernier message utilisateur.
- **Estimation de tokens & persistance incrémentale** :
  - Calcule `input_tokens`, `thought_tokens`, `text_tokens` et `total_tokens` (~1 token / 4 octets + 258 tok/image) et les attribue au **modèle indiqué par Google** (`inner[39]`/`inner[42]`).
  - Persiste sur disque (`0600`) : `*.calls.jsonl` (historique 12 h), `*.tokens.jsonl` (journal continu append-only), `*.flights.json` (5 dernières conversations / max 200 demandes).
- **Conversations temporaires à chaud (`inner[45]=1`)** :
  - Activables et désactivables en un clic depuis GMINI Control (`/_ui`) sans redémarrer le proxy ; l'état est conservé dans `.state.json`.

---

## 3. Installation, compilation & mise en route

### Prérequis
- **Go 1.22+** (zéro dépendance externe, compilable entièrement hors réseau).
- Un compte Google connecté sur [gemini.google.com](https://gemini.google.com) (pour obtenir les cookies de session).

### Compiler et lancer les tests (hors réseau)

```bash
git clone https://github.com/nioyco/GMINI-Web-Proxy.git
cd GMINI-Web-Proxy

# Suite de tests complète hors réseau
HTTPS_PROXY=http://127.0.0.1:9 HTTP_PROXY=http://127.0.0.1:9 GOPROXY=off go test -count=1 ./...

# Compilation du binaire statique (droits restreints 0700 recommandés)
go build -o gmini-web-proxy .
chmod 0700 gmini-web-proxy
```

### Préparer la configuration & les cookies

1. Créer un dossier privé (ex. sur macOS : `"$HOME/Library/Application Support/go-gemini-web2api"`, droits `0700`).
2. Copier `.env.example` vers `.env` (`chmod 0600 .env`) et renseigner au minimum :
   - `GEMINI_API_KEYS=<une-clé-locale-aléatoire>` (ex. `openssl rand -hex 24`)
   - `GEMINI_COOKIE_FILE=<chemin-vers-cookie.txt>`
3. Récupérer vos cookies de session sur `https://gemini.google.com` (DevTools → Application → Cookies) et les enregistrer sur une seule ligne dans `cookie.txt` (`chmod 0600 cookie.txt`) :
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

### Mise en route quotidienne avec Antigravity (macOS)

Avec la console d'administration dédiée **`GMINI-Proxy-Control_V2.0.0.command`** (interface BIOS 2 colonnes sans saut, état complet permanent à droite, navigation par sous-menus `↑`/`↓`/`Entrée`/`←`) :
1. Ouvrir la console : vérifier dans la colonne de droite le bandeau **`● EN SERVICE`** / **`■ ARRÊTÉ`**, le **Cookie** (`présent`, `0600`) et la synchronisation **Officielle ↔ Clone P1/P5a** (`v2.32.1`, `relais : oui`).
2. Sous-menu **Proxy GMINI** → *Démarrer le proxy GMINI*, puis *Ouvrir GMINI Control* (`http://127.0.0.1:8081/_ui`) et dérouler la checklist (§6).
3. Sous-menu **Modèles Antigravity** *(séparé)* → *Synchroniser les modèles* (aperçu ou appliquer avec sauvegarde `0600`) ou *Restaurer les modèles depuis une sauvegarde*.
4. Sous-menu **Antigravity (IDE & Mises à jour)** → *Lancer Antigravity CLONE patché (P1/P5a)* — toujours **une seule** instance Antigravity ouverte à la fois.

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
| `GET` | `/_ui` | Local | Tableau de bord **GMINI Control** (génère un jeton de session `X-GMINI-UI`) |
| `GET` | `/_ui/api/state` | `X-GMINI-UI` | Snapshot temps réel pour l'interface `/_ui` |
| `POST` | `/_ui/api/models/refresh` | `X-GMINI-UI` | Rafraîchir le catalogue depuis `/_ui` |
| `POST` | `/_ui/api/cookie-refresh/toggle` | `X-GMINI-UI` | Activer/désactiver l'actualisation auto du cookie (persisté) |
| `POST` | `/_ui/api/cookie-refresh/run` | `X-GMINI-UI` | Déclencher une actualisation manuelle du cookie (cooldown 3 min) |
| `POST` | `/_ui/api/temporary-chat/toggle` | `X-GMINI-UI` | Activer/désactiver les conversations temporaires `inner[45]=1` (persisté) |
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

### Streaming, outils & garde-fous mécaniques (K9 – K24)

| Variable | Défaut | Description |
|---|---|---|
| `GEMINI_SSE_KEEPALIVE_SEC` | `15` | Fréquence des battements SSE `: keepalive` (secondes). |
| `GEMINI_WAIT_NOTE_SEC` | `15` | Intervalle des notes d'attente dans `reasoning_content` (`0` = désactivé). |
| `GEMINI_SILENCE_WARN_SEC` | `45` | Seuil d'alerte log si Google reste muet après les pensées. |
| `GEMINI_NO_CONTENT_SEC` | `0` | Timeout si aucun contenu n'arrive (`0` = désactivé ; `240` recommandé dans `.env.example` ; pilotable à chaud dans `/_ui` `#nc-toggle` & `Control V2.0.0`, écriture `.env` `0600`). |
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
| `GEMINI_INTERNAL_TOOL_ACTION` | `notify` | Action sur outil interne détecté : `notify` \| `retry` \| `off`. |
| `GEMINI_CANNED_ERROR_RETRY` | `off` | Relance sur erreur interne Google (`inner[31]=true`) : `off` \| `once` (K15 ; pilotable à chaud via `/_ui` `#cer-toggle` & `Control V2.0.0`, écriture `.env` `0600` — K24). |
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
| `GEMINI_FLIGHTS_FILE` | `<state>.flights.json` | Persistance des 5 dernières conversations (`0600`). |
| `GEMINI_TRACE_DIR` | *(vide)* | Dossier de traces complètes opt-in (`0600`, contient prompts/réponses, jamais de cookies). |
| `GEMINI_TRACE_MAX_FILES` | `0` | Nombre max de fichiers de traces conservés (`0` = valeur interne). |
| `GEMINI_TRACE_RAW` | `false` | Enregistre aussi les trames `wrb.fr` brutes dans les traces. |

---

## 6. Checklist de vérification sur GMINI Control (`/_ui`)

Avant de lancer un lot important dans Antigravity, ouvrir **`http://127.0.0.1:8081/_ui`** et vérifier dans l'ordre :

1. **Bandeau supérieur** : aucune bannière rouge (*Proxy injoignable* / *Le proxy a redémarré*) ni orange (*Identité non garantie* / *.env modifié*). Si `.env` a changé, cliquer sur **« Recharger .env »**.
2. **Tuile « Session Google »** :
   - Badge vert **`Connectée`** (jamais `Non connectée` ni `Session affaiblie`).
   - **Âge du jeton** `< 90 min`, ou **Actualisation auto activée** (au repos, le message `en veille (calme) : s'actualisera automatiquement à la prochaine demande` garantit un rafraîchissement automatique avant le prochain appel).
   - Sinon, cliquer sur **« Actualiser le cookie maintenant »**.
3. **Tuile « Proxy & Catalogue »** :
   - **`4 modèles`** (`dynamic` ou `persisted`), **Blocage anti-robot : `Aucun`**.
4. **Tuile « Garde-fous & Compteurs »** :
   - `Appels non convertis` = `0`, `Action non vérifiée` = `0`, `Réponses vides Google` = `0`, `Raccourci code_reference` = `on`.
5. **Tuiles « Réglages .env » & « Journal & Confidentialité »** :
   - `code_ref_fastpath: on`, `no_internal_code_exec_rule: on`, `tool_audit_note / split_rw: on / on`.
   - Bascule **« Conversations temporaires »** selon votre préférence (`Activer` / `Désactiver` à chaud).
6. **Tableau « Modèles détectés chez Google »** :
   - Les 4 modèles apparaissent avec leurs noms confirmés (`3.5 Flash-Lite`, `4 Hydrogen`, `3.1 Pro`, **`4 Argon`**) et la colonne « Qui répond » est en vert (`Indiqué par Google`).
7. **Tableaux « Antigravity sur ce Mac » & « Correspondance avec Antigravity »** :
   - La copie ouverte affiche **Modèles du proxy : `Oui`** et **Relais anti-coupure : `Oui`** (indispensable au-delà de 300 s).
   - Toutes les entrées de correspondance sont **`Dans le catalogue`**.

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
| `GMINI : Google a renvoyé une réponse vide sur <modèle> (prompt de N Ko)` | Texte final (K16) | Flux clos sans candidat ni texte (aucune relance auto). Compacter la conversation (`/compact`), réessayer ou changer de modèle. |
| `GMINI : Google a renvoyé une erreur interne sur <modèle>` | Texte final (K15) | Erreur interne Google (`inner[31]=true`). Réessayer ou ouvrir une nouvelle conversation. |
| `GMINI : Gemini a refusé ou simulé deux fois sans appeler les outils locaux` | Texte final (K19/K20) | Deux refus/simulations consécutifs. Ouvrir une nouvelle conversation (l'historique long a biaisé le modèle). |
| `GMINI : la réponse de Gemini s'emballe (N Ko sans fin) ; demande interrompue` | Texte final (K9) | Garde-fou `early_stop` (`256 Ko` ou `120 s` sans `tool_call` complet). Réessayer ou réduire le prompt. |

---

## 8. Emplacements des fichiers & commandes de supervision

### Carte complète des fichiers (installation standard macOS)

| Type | Emplacement par défaut | Contient des secrets / texte ? | Rôle |
|---|---|---|---|
| **Journal principal** | `$HOME/Library/Logs/gmini/proxy.log` | Non (métadonnées uniquement) | Démarrages, `request_end`, `served_model`, `code_ref_fastpath`, `upstream_silence_after_thoughts`, erreurs, interceptions |
| **Traces complètes** | `$HOME/Library/Logs/gmini/traces/*.json` (`0600`) | **Oui : prompts, pensées, réponses et trames** (jamais de cookies ni clés) | Analyse fine d'un échange, inspection des `tool_call` et des latences trame par trame |
| **Journal des appels (12 h)** | `$HOME/Library/Application Support/go-gemini-web2api/cookie.txt.calls.jsonl` | Non (`0600`, tailles/délais/identités/tokens) | Alimente « Derniers appels (12 h) » et synchronise les noms confirmés (`4 Argon`) |
| **Journal continu tokens** | `$HOME/Library/Application Support/go-gemini-web2api/cookie.txt.tokens.jsonl` | Non (`0600`, append-only) | Historique cumulé des tokens estimés par modèle indiqué par Google |
| **Historique demandes** | `$HOME/Library/Application Support/go-gemini-web2api/cookie.txt.flights.json` | Non (`0600`, 5 dernières conv. / max 200) | Persiste « Demandes en cours » entre redémarrages |
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

# 5. Afficher les 15 derniers appels enregistrés dans .tokens.jsonl (sans aucun secret)
tail -n 15 "$HOME/Library/Application Support/go-gemini-web2api/cookie.txt.tokens.jsonl" | python3 -c "import sys,json
for l in sys.stdin:
    d=json.loads(l)
    print(f\"{d['time'][11:19]} | {d['effective_model']:16s} | ind={str(d['google_indicated']):5s} | tot={d['total_tokens']:6d} (E:{d['input_tokens']} P:{d['thought_tokens']} R:{d['text_tokens']}) | err={d.get('error','')}\")"

# 6. Vérifier le processus actif et le port d'écoute 8081
lsof -nP -iTCP:8081 -sTCP:LISTEN
```

---

## 9. Clients compatibles

### Antigravity (IDE)
- Base URL : `http://127.0.0.1:8081/v1` (API compatible OpenAI, streaming SSE + outils).
- Les modèles du catalogue (`/v1/models`) sont déclarés dans `custom_models.json` via l'Enabler P1/P5a, qui fournit également le relais anti-coupure > 300 s (`relayKeepaliveMs` / `firstKeepaliveDelayMs`).

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
| **Session `Non connectée` ou repli silencieux sur un autre modèle (`other_model`)** | Cookie Google expiré ou affaibli (`__Secure-1PSIDTS` ancien) | 1. Dans GMINI Control : cliquer **« Actualiser le cookie maintenant »**.<br>2. Si le rafraîchissement silencieux échoue 2 fois : reconnecter le compte Google dans Chrome et réexporter `cookie.txt`.<br>3. Redémarrer le proxy et vérifier le badge vert `Connectée`. |
| **Blocage anti-robot (`/sorry` / `google_blocked`)** | Trop d'appels rapprochés ou rafraîchissements en boucle | **Ne pas relancer le proxy en boucle.** Attendre la fin du backoff affichée dans GMINI Control (`5 → 60 min`). Vérifier `GEMINI_COOKIE_ROTATION=false` et `GEMINI_AUTH_RECOVERY=false`. |
| **Coupure systématique au bout de 300 s (5 min)** | L'instance Antigravity ouverte n'a pas le relais keepalive | Fermer Antigravity (`Cmd+Q`), vérifier dans GMINI Control (« Antigravity sur ce Mac ») que la copie lancée a **Relais anti-coupure : `Oui`**. |
| **GMINI Control affiche `401` / `Le proxy a redémarré`** | Onglet ouvert avant un redémarrage (jeton `X-GMINI-UI` régénéré) | Cliquer sur **« Recharger la page »** (`Cmd+R`). Depuis `v1.2.0`, le polling s'arrête automatiquement sur `401`. |
| **Port `8081` occupé au démarrage** | Ancien processus proxy encore actif | Vérifier `lsof -nP -iTCP:8081 -sTCP:LISTEN`, arrêter proprement (`kill -INT <pid>`), puis redémarrer. |
| **Modèle muet, `canned_error` (`inner[31]=true`) ou réponse vide** | Prompt devenu trop volumineux (`> 300 Ko`, affiché en orange) ou incident Google ponctuel | 1. Compacter la conversation (`/compact`) ou ouvrir une nouvelle conversation.<br>2. Vérifier le quota réel sur `gemini.google.com/usage`.<br>3. Basculer temporairement sur un autre modèle du catalogue. |
| **Le modèle prétend avoir modifié un fichier sans rien changer (ou alerte `⚠ GMINI vérification mécanique`)** | Réponse texte sans `tool_call` | 1. Ne pas valider le résultat.<br>2. Contrôler le « Bilan vérifié des outils » dans les pensées et « Demandes en cours ».<br>3. Redemander explicitement : *« Appelle `write_file` / `edit_file` sur `<chemin>` maintenant, ne colle pas le contenu dans le chat »*. |
| **Pause longue de 20–60 s sur `code_reference`** | Gemini tente son bac à sable de code interne | Vérifier dans « Réglages .env » que `code_ref_fastpath` et `no_internal_code_exec_rule` sont sur **`on`**. Sur un bloc multi-commandes complexe, le proxy laisse prudemment Gemini émettre ses `tool_call` structurés. |
| **Mauvais nom de modèle dans Antigravity (ex. `4 Carbon` au lieu de `4 Argon`)** | `custom_models.json` conserve l'ancien libellé | 1. Vérifier `4 Argon` dans `/_ui` (`v1.2.0+`).<br>2. Fermer Antigravity (`Cmd+Q`) et relancer la synchronisation des modèles (avec sauvegarde `0600`). |
| **Échec de migration vers une nouvelle version officielle d'Antigravity (`UNSUPPORTED_RUNTIME`)** | Antigravity a modifié la structure interne de son `app.asar` (ex. v2.32.1 : `dist/lsDefaults.js` étalé dans `dist/languageServer.js`) | L'Enabler refuse par sécurité sans modifier l'app officielle ni le clone précédent. Continuer sur le dernier clone fonctionnel en attendant l'adaptation de `scripts/runtime-patch.mjs`. |

---

## 11. Sécurité & confidentialité

- **Écoute locale stricte** : le proxy écoute sur `127.0.0.1:8081` par défaut et refuse de démarrer sans `GEMINI_API_KEYS` (sauf `GEMINI_ALLOW_NO_AUTH=1`).
- **Permissions restrictives** : les binaires sont en `0700` et tous les fichiers sensibles ou journaux générés (`.env`, `cookie.txt`, `.state.json`, `.calls.jsonl`, `.tokens.jsonl`, `.flights.json`, traces) sont écrits en `0600`.
- **Aucun secret dans les journaux** : `proxy.log`, `/_status`, `/_ui`, `.calls.jsonl`, `.tokens.jsonl` et `.flights.json` ne contiennent ni cookies, ni clés API, ni contenu de conversation.
- **Règle d'hygiène Git** : `.gitignore` exclut automatiquement `.env`, `*.txt`, `*.state.json`, `*.calls.jsonl`, `*.tokens.jsonl`, `*.flights.json` et `*.bak.*`.

---

## 12. Crédits & origine du projet

- Ce dépôt est dérivé et profondément étendu à partir du projet open source **[`n0madic/go-gemini-web2api`](https://github.com/n0madic/go-gemini-web2api)** (licence MIT).
- Inspirations protocolaires initiales : [`Sophomoresty/gemini-web2api`](https://github.com/Sophomoresty/gemini-web2api) et [`HanaokaYuzu/Gemini-API`](https://github.com/HanaokaYuzu/Gemini-API).
- Licence : [`LICENSE`](LICENSE) (MIT).
