---
tags:
  - Setup
  - Configuration
---

# Setup Webhook

This page shows how to set up a webhook for your deployments.

!!! question "About Webhooks"

     Webhooks are event-based triggers that notify doco-cd when there are changes in the repositories to deploy. This is the recommended way to trigger deployments as it is more efficient and faster than polling but requires doco-cd to be reachable from the internet (or local network if you self-host your Git provider) and some setup on your Git provider.

## Webhook Endpoint

To enable the webhook endpoint, you need to set the `WEBHOOK_SECRET` [environment variable](App-Settings.md#available-settings) to a secure secret value and publish the webhook port  (default is `80`, see the `HTTP_PORT` [environment variable](App-Settings.md#available-settings)) in the doco-cd `docker-compose.yml` file.

You can use tools like [pwgen](https://linux.die.net/man/1/pwgen) or [openssl](https://www.openssl.org/) to generate a random secret for the `WEBHOOK_SECRET`.

=== "pwgen"
    ```sh title="Generate a random password with pwgen"
    pwgen -s 40 1
    ```

=== "openssl"
    ```sh title="Generate a random password with openssl"
    openssl rand -base64 40
    ```

Doco-CD then listens on the URL path `/v1/webhook` for incoming webhooks (http requests).
The full url would look like this for example: `https://your-server.com/v1/webhook`
I recommend that you use HTTPS so that your secret key is transmitted in encrypted form.

To find more information about the webhook endpoint, see the [Webhook Listener Endpoint](Endpoints/Webhook-Listener.md).

## Setup in Git Providers

=== "GitHub"

    1. Go to your repository settings.
    2. Click on `Webhooks`.
    3. Click on the `Add webhook` button.
    4. Fill in the following fields:
         - **Payload URL**: The URL to the app endpoint, e.g. `https://example.com/v1/webhook`.
         - **Content type**: Set this to `application/json`.
         - **Secret**: The `WEBHOOK_SECRET` you have set in your app configuration (See [App Settings](App-Settings.md)).
         - **SSL verification**: Enable this if you have a valid SSL certificate for the app endpoint.
         - **Which events would you like to trigger this webhook?**: Set this to `Just the push event` or `Let me select individual events` and then `Pushes` and/or `Branch or tag creation`.
         - **Active**: Enable this to activate the webhook.
    
    !!! note
        GitHub webhooks are [limited to 10 seconds](https://docs.github.com/en/webhooks/testing-and-troubleshooting-webhooks/troubleshooting-webhooks#timed-out) for the delivery timeout. 
        If you use the [`wait=true`](Endpoints/Webhook-Listener.md#wait) query parameter and your deployment takes longer than this (e.g., when pulling big images), the webhook will appear as failed in the GitHub UI, but the deployment will still continue.
        You can check the status of the deployment in the doco-cd logs.
    
    
    ### Custom webhook using Github actions
    
    If you need more control over the webhook delivery, you can use a custom GitHub Action to send the webhook.
    
    Here is an example of a GitHub Action workflow that sends a webhook to the doco-cd endpoint: 
    
    ```yaml title=".github/workflows/send-webhook.yml"
    name: Send Dynamic Push Event Webhook
    
    on:
      repository_dispatch: # replace with your desired trigger event
        types: [docker_publish_done]
    
    jobs:
      send-push-event:
        runs-on: ubuntu-latest
        steps:
          - name: Send push event to webhook (with HMAC SHA256 signature)
            env:
              WEBHOOK_URL: ${{ secrets.WEBHOOK_URL }}
              WEBHOOK_SECRET: ${{ secrets.WEBHOOK_SECRET }}
            run: |
              CLONE_URL="https://github.com/${GITHUB_REPOSITORY}.git"
              PAYLOAD=$(jq -n \
                --arg ref "${GITHUB_REF}" \
                --arg before "${GITHUB_SHA}" \
                --arg after "${GITHUB_SHA}" \
                --arg clone_url "$CLONE_URL" \
                --arg reponame "${GITHUB_REPOSITORY##*/}" \
                --arg repo_fullname "${GITHUB_REPOSITORY}" \
                --arg pusher_name "${GITHUB_ACTOR}" \
                --arg pusher_email "noreply@github.com" \
                '{
                  ref: $ref,
                  before: $before,
                  after: $after,
                  repository: {
                    name: $reponame,
                    full_name: $repo_fullname,
                    clone_url: $clone_url
                  },
                  pusher: {
                    name: $pusher_name,
                    email: $pusher_email
                  }
                }'
              )
    
              SIGNATURE="sha256=$(echo -n "$PAYLOAD" | openssl dgst -sha256 -hmac "$WEBHOOK_SECRET" | sed 's/^.* //')"
    
              curl -v -X POST "$WEBHOOK_URL" \
                -H "X-GitHub-Event: push" \
                -H "Content-Type: application/json" \
                -H "X-Hub-Signature-256: $SIGNATURE" \
                -d "$PAYLOAD"
    ```
    
    Trigger this workflow by sending a `repository_dispatch` event with the type `docker_publish_done` (or your desired event type).
    For example in your main workflow job add this step:
    
    ```yaml title=".github/workflows/main.yml"
    - name: Notify by repository dispatch
      env:
        GH_TOKEN: ${{ secrets.REPO_GITHUB_TOKEN }}
      run: |
        curl -X POST \
          -H "Authorization: Bearer $GH_TOKEN" \
          -H "Accept: application/vnd.github+json" \
          https://api.github.com/repos/${{ github.repository }}/dispatches \
          -d '{"event_type":"docker_publish_done"}'
    ```
    
    Original post: https://github.com/kimdre/doco-cd/discussions/798#discussioncomment-15048816

=== "Gitea and Forgejo"

    1. Go to your repository settings.
    2. Click on `Webhooks`.
    3. Click on the `Add webhook` button and select `Gitea` in the dropdown.
    4. Fill in the following fields:
        - **Target URL**: The URL to the app endpoint, e.g. `https://example.com/v1/webhook`.
        - **HTTP Method**: Set to `POST`.
        - **POST Content Type**: Set to `application/json`.
        - **Secret**: The `WEBHOOK_SECRET` you have set in your app configuration (See [App Settings](App-Settings.md)).
        - **Trigger On**: Set this to `Push Events` or `Custom Events...` and then `Create` and/or `Push`.
        - **Branch filter**: Set this to the branch you want to trigger the webhook or `*` for all branches.
        - **Active**: Enable this to activate the webhook.
      
    !!! note
        If you use the [`wait=true`](Endpoints/Webhook-Listener.md#wait) query parameter, you may need to extend the webhook delivery timeout in the Gitea config file `app.ini` to allow for longer running deployments (e.g., when pulling big images) 
        This can be done by setting the `DELIVER_TIMEOUT` variable in the `[webhook]` section of the config file. You may want to increase this to 30 seconds or more depending on your deployment time.
  
        In addition, you may need to set the `ALLOWED_HOST_LIST` variable in the `[webhook]` section of the config file to allow the webhook to be delivered to the doco-cd webhook endpoint.
    
    
    **Allow access to the webhook endpoint**
    
    If you have issues reaching the app endpoint from Gitea, you may need to allow the domain or ip address of the app endpoint in the Gitea configuration:
    
    - Open the **app.ini** for Gitea, typically found at `/etc/gitea/conf/app.ini`, and add the environment of the app endpoint to the allowed webhooks list:
      ```ini
      [webhook]
      ALLOWED_HOST_LIST = doco-cd.example.com
      ```
      Replace `doco-cd.example.com` with the domain or ip address of the app endpoint. You can also use wildcards like `*.example.com`
    - Restart Gitea to apply the changes to the configuration.

=== "Gitlab"
    
    1. Go to your repository settings.
    2. Click on `Webhooks`.
    3. Click on the `Add new webhook` button.
    4. Fill in the following fields:
         - **URL**: The URL to the app endpoint, e.g. `https://example.com/v1/webhook`.
         - **Secret Token**: The `WEBHOOK_SECRET` you have set in your app configuration (See [App Settings](App-Settings.md)).
         - **Trigger**: Set this to `Push events` and/or `Tag push events`.
         - **SSL verification**: Enable this if you have a valid SSL certificate for the app endpoint.
    5. Click on the `Test` button and then `Push events` to test the webhook.
    
    !!! note
        If you use the [`wait=true`](Endpoints/Webhook-Listener.md#wait) query parameter, you may need to extend the webhook delivery timeout in the Gitlab config file `gitlab.rb` to allow for longer running deployments (e.g., when pulling big images)
        This can be done by setting the `webhook_timeout` variable in the `gitlab.rb` file. You may want to increase this to 30 seconds or more depending on your deployment time.


=== "Azure DevOps"
    
    !!! warning "Azure Devops native webhooks (Service Hooks) are not supported"
        Azure Devops native webhooks (_Service Hooks_) are not supported because they lack important data in the payload and security best practices (no HMAC signature, no token header, only simple basic auth). 
        If users want to send webhooks from Azure DevOps, they need to build and send the webhook payload themselves using something like curl in a CI pipeline.
    
    !!! tip
        See the [Custom webhook using Github actions](#custom-webhook-using-github-actions) section in the [Github](#setup-in-git-providers-github) tab for an example of how to build and send the webhook payload with a HMAC SHA256 signature.
     
    A possible pipeline config could look like this (not tested, please leave me feedback if this works for you):
    
     1. Create a new pipeline in your Azure DevOps project.
     2. Add the following pipeline variables (mark them as secret):
        - `WEBHOOK_URL`: The URL to the app endpoint, e.g. `https://example.com/v1/webhook`.
        - `WEBHOOK_SECRET`: The `WEBHOOK_SECRET` you have set in your app configuration (See [App Settings](App-Settings.md)).
     3. Add the following YAML configuration to the pipeline:
         ```yaml title="azure-pipelines.yml"
         trigger:
         - main
      
         pool:
           vmImage: ubuntu-latest
      
         #variables:
           # Define these in the pipeline UI or variable group and mark as secret
           # WEBHOOK_URL: (secret)
           # WEBHOOK_SECRET: (secret)
      
         steps:
           - script: |
               set -e
      
               CLONE_URL="$(Build.Repository.Uri)"
               REPO_FULLNAME="$(Build.Repository.Name)"
               REPO_NAME="$(basename "$REPO_FULLNAME")"
      
               PAYLOAD=$(jq -n \
                 --arg ref "$(Build.SourceBranch)" \
                 --arg before "$(Build.SourceVersion)" \
                 --arg after "$(Build.SourceVersion)" \
                 --arg clone_url "$CLONE_URL" \
                 --arg reponame "$REPO_NAME" \
                 --arg repo_fullname "$REPO_FULLNAME" \
                 --arg pusher_name "$(Build.RequestedFor)" \
                 --arg pusher_email "noreply@azuredevops.com" \
                 '{
                   ref: $ref,
                   before: $before,
                   after: $after,
                   repository: {
                     name: $reponame,
                     full_name: $repo_fullname,
                     clone_url: $clone_url
                   },
                   pusher: {
                     name: $pusher_name,
                     email: $pusher_email
                   }
                 }'
               )
      
               SIGNATURE="sha256=$(echo -n "$PAYLOAD" | openssl dgst -sha256 -hmac "$WEBHOOK_SECRET" | sed 's/^.* //')"
      
               echo $PAYLOAD | jq
               echo $SIGNATURE
      
               curl -v -X POST "$WEBHOOK_URL" \
                 -H "X-GitHub-Event: push" \
                 -H "Content-Type: application/json" \
                 -H "X-Hub-Signature-256: $SIGNATURE" \
                 -d "$PAYLOAD"
             displayName: Send push event webhook
             env:
               WEBHOOK_URL: $(WEBHOOK_URL)
               WEBHOOK_SECRET: $(WEBHOOK_SECRET)
         ```
