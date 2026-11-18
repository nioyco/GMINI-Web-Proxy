# Historique des versions — GMINI-Web-Proxy

Ce fichier recense les versions publiées du proxy **GMINI-Web-Proxy** (dérivé de `n0madic/go-gemini-web2api`) et leurs jalons techniques (`P6`–`K20`).

---

## v1.2.0 — 2026-11-18 (Lot K20, suivi tokens & confidentialité temps réel)

### Nouveautés & garde-fous mécaniques (K20)
- **Audit mécanique des outils & lutte anti-mensonge** (`pkg/apiconv/toolaudit.go`, `GEMINI_TOOL_AUDIT_NOTE=true`) :
  - Détecte automatiquement les faux dumps de fichiers en texte brut sans `tool_call` (`IsEarlyFakeWriteOpener`) et les affirmations non vérifiées d'écriture disque ou de lancement de tests (`DetectUnverifiedActionClaim`).
  - Relance automatiquement une fois avec consigne corrective, et ajoute un bandeau visible `⚠ GMINI (vérification mécanique)` dans la réponse finale si l'anomalie persiste.
  - Injecte en fin de tour un **« Bilan vérifié des outils »** infalsifiable dans les pensées (`reasoning_content`) listant les outils réellement transmis par le proxy depuis le dernier message utilisateur.
- **Séparation automatique des lots `read_file` + `write_file`** (`GEMINI_SPLIT_READ_WRITE_BATCH=true`) : retient `write_file` lorsqu'il est émis dans le même lot qu'un `read_file` avant lecture effective, sans bloquer les lots légitimes `read_file` + `edit_file`.
- **Synchronisation automatique des noms réels confirmés dans le catalogue** (`pkg/gemini/models.go`, `served.go`) : le nom confirmé par Google dans `inner[42]` (ex. `95a4c0abcba24364` → **`4 Argon`**) remplace automatiquement l'ancien libellé renvoyé par `otAQ7b` (`4 Carbon`), y compris dans `/v1/models` et `.state.json`.
- **Suivi des tokens estimés par modèle indiqué par Google & persistance incrémentale** (`pkg/gemini/tokens.go`) :
  - Estimation (~1 token / 4 octets + 258 tok/image) attribuée au modèle réellement indiqué par Google (`inner[39]`/`inner[42]`).
  - Persistance incrémentale `0600` sur disque : `*.calls.jsonl` (12 h), `*.tokens.jsonl` (append-only, backfill automatique), `*.flights.json` (5 dernières conversations, max 200 lignes).
- **Conversations temporaires pilotables à chaud (`inner[45]=1`)** : bouton `Activer` / `Désactiver` dans GMINI Control (`POST /_ui/api/temporary-chat/toggle`), conservé au redémarrage dans `.state.json`.
- **Clarté de l'actualisation auto au repos** : remplace le compteur trompeur `0 s` par `en veille (calme) : s'actualisera automatiquement à la prochaine demande` (`DeferredReason`).

### Correctifs techniques
- **SSE idempotent (`openSSE`)** : supprime le doublon `WriteHeader` lorsque le flux SSE est déjà ouvert.
- **Repli modèle vs notes d'attente** : sépare `streamToolsResult.modelForwarded` des notes techniques émises à 2 s pour ne plus bloquer le repli/retry légitime.
- **Retry K19/K20 unifié via `runStreamTools`** : vérifie également les pensées (`thought`) sur la 2e tentative.
- **GMINI Control (`ui.html`)** : arrêt automatique du polling sur HTTP `401` avec bouton « Recharger la page », regroupement des tuiles de statut, et correction du débordement horizontal dans la tuile *Appels de fond & Rotation* (`min-width: 0; overflow-wrap: anywhere`).
- **Dépôt & module Go** : module renommé en `github.com/nioyco/GMINI-Web-Proxy`, `.gitignore` durci contre toute fuite de secrets/états, guide d'exploitation complet intégré (`docs/GUIDE_COMPLET_EXPLOITATION_GMINI.md`).
- **Compatibilité Antigravity `v2.32.1+`** : validée en production (> 500 s continus sans coupure) avec l'Enabler P1/P5a (`ad28f7e`, support de `dist/lsDefaults.js` / `baseLsArgs`) et la console d'administration `GMINI-Proxy-Control v2.0.0`.
- **Étude R&D-21** : diagnostic complet des `bash?code_reference` après le tour 1 (position du rappel avant `<USER_REQUEST>`), de la latence `3.1 Pro` `thinking_level=4` (`32,7 % > 60 s`) et de l'ordonnancement `served_model` / `request_end`.

---

## v1.1.0 — 2026-11-16 → 2026-11-17 (Lots K14 – K19)

- **K19** : détection des quotas d'outils inventés et des rapports simulés jusque dans les pensées (`thought`) + relance corrective automatique.
- **K18** :
  - **T1** : raccourci **`code_ref_fastpath`** (`GEMINI_CODE_REF_FASTPATH=true`) convertissant immédiatement un bloc ```` ```bash?code_reference ```` en `tool_call` `run_command` sans subir la pause du bac à sable interne Google.
  - **T3 / T4** : actualisation automatique du cookie **`on_demand`** via commande externe (`GEMINI_COOKIE_REFRESH_CMD`), état persisté dans `.state.json`, déclenchement manuel avec cooldown depuis `/_ui`.
  - **T5** : détection des modifications de `.env` depuis le démarrage + rechargement à chaud des réglages non-secrets (`POST /_ui/api/config/reload`).
- **K17** : règle anti-exécution de code interne (`GEMINI_NO_INTERNAL_CODE_EXEC_RULE=true`), surveillance fine des outils internes (`GEMINI_INTERNAL_TOOL_ACTION=notify|retry|off`).
- **K16** : détection propre des réponses vides Google (`upstream_empty_reply`) sans relance aveugle consommant du quota.
- **K15** : détection des erreurs internes Google (`inner[31]=true` / `canned_error`), relance optionnelle bornée (`GEMINI_CANNED_ERROR_RETRY`).
- **K14** : **sobriété anti-robot** — suppression des appels de fond en période calme (`GEMINI_IDLE_QUIET_MIN=10`), rotation cookie et reprise auth désactivées par défaut, backoff exponentiel sur `/sorry` (`5 → 60 min`), persistance d'état `.state.json` pour éviter de recharger `/app` à chaque redémarrage.

---

## v1.0.0 — 2026-11-13 → 2026-11-15 (Lots P6 – K13)

- **Sécurité par défaut** : écoute restreinte à `127.0.0.1:8081` (même pour `:8081` ou un port nu), clé API `GEMINI_API_KEYS` obligatoire au démarrage sauf `GEMINI_ALLOW_NO_AUTH=1`, arrêt propre (`SIGINT`/`SIGTERM`), protection SSRF sur images distantes.
- **Interface locale GMINI Control (`/_ui`)** : tableau de bord temps réel protégé par jeton de session (`X-GMINI-UI`), suivi des demandes en cours, détection des copies Antigravity sur macOS et correspondance avec `custom_models.json`.
- **Identité réelle du modèle** : lecture en direct de `inner[39]` (id hex) et `inner[42]` (nom annoncé par Google), alerte sur repli silencieux (`other_model` / `no_thoughts`).
- **Fiabilité streaming & outils (K8–K13)** : keepalives SSE toutes les 15 s, diffusion en direct des pensées (`reasoning_content`), notes techniques d'attente (`2 s` / `15 s`), avertissement de silence après pensées (`45 s`), filtre anti-écho d'historique, garde-fou anti-emballement `early_stop` (`256 Ko` / `120 s`), traces complètes opt-in (`GEMINI_TRACE_DIR`, `0600`).

---

## v0.1.0 — Base initiale

- Base Go zéro-dépendance issue de [`n0madic/go-gemini-web2api`](https://github.com/n0madic/go-gemini-web2api) : passerelle Gemini Web `StreamGenerate` → API OpenAI, Anthropic, Responses et Google native.
