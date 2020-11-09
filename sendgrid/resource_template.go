package sendgrid

import (
        "encoding/json"
        "fmt"
        "log"
        "net/http"
        "strings"
        "time"

        "github.com/hashicorp/terraform-plugin-sdk/helper/resource"
        "github.com/hashicorp/terraform-plugin-sdk/helper/schema"
        "github.com/pkg/errors"
        "github.com/sendgrid/sendgrid-go"
)

var (
    createTemplateRate = time.Tick(5 * time.Second)
    deleteTemplateRate = time.Tick(5 * time.Second)
)

type template struct {
    Name        string `json:"name"`
    TemplateID  string `json:"id"`
    Versions    []versions `json:"versions"`
}

type versions struct {
    Active             int    `json:"active"`
    Editor             string `json:"editor"`
    Id                 string `json:"id"`
    GeneratePlainContent bool `json:"generate_plain_content"`
    HtmlContent        string `json:"html_content"`
    Name               string `json:"name"`
    PlainContent       string `json:"plain_content"`
    Subject            string `json:"subject"`
    TemplateId         string `json:"template_id"`
    ThumbnailUrl       string `json:"thumbnail_url"`
}

func resourceTemplate() *schema.Resource {
    return &schema.Resource{
        Create: resourceTemplateCreate,
        Delete: resourceTemplateDelete,
        Read:   resourceTemplateRead,
        Update: resourceTemplateUpdate,
        Importer: &schema.ResourceImporter{
            State: schema.ImportStatePassthrough,
        },
        Schema: map[string]*schema.Schema{
            "name": &schema.Schema{
                Type:     schema.TypeString,
                Required: true,
            },
            "versions": &schema.Schema{
                Type:     schema.TypeList,
                Optional: true,
                MaxItems: 300,
                MinItems: 0,
                Elem: &schema.Resource{
                    Schema: map[string]*schema.Schema{
                        "name": &schema.Schema{
                            Type:     schema.TypeString,
                            Required: true,

                        },
                        "id": &schema.Schema{
                            Type:     schema.TypeString,
                            Optional: true,
                        },
                        "subject": &schema.Schema{
                            Type:     schema.TypeString,
                            Required: true,
                        },
                        "html_content": &schema.Schema{
                            Type:     schema.TypeString,
                            Optional: true,
                        },
                        "plain_content": &schema.Schema{
                            Type:     schema.TypeString,
                            Optional: true,
                        },
                        "active": &schema.Schema{
                            Type:     schema.TypeInt,
                            Optional: true,
                        },
                    },
                },
            },
       },
    }
}

func resourceTemplateCreate(d *schema.ResourceData, m interface{}) error {
    d.Partial(true)
    name := d.Get("name").(string)

    payload := map[string]interface{}{
        "name": name,
    }

    data, err := json.Marshal(payload)
    if err != nil {
        return err
    }

    apiKey := m.(*Config).APIKey
    request := sendgrid.GetRequest(apiKey, "/v3/templates", sendgridAddress)
    request.Method = http.MethodPost
    request.Body = data

    res, err := doRequest(request, withStatus(http.StatusCreated), withRateLimit(createTemplateRate))
    if err != nil {
        return errors.Wrap(err, "failed to create template")
    }

    var t template
    err = json.Unmarshal([]byte(res.Body), &t)
    if err != nil {
        return errors.Wrap(err, "failed to unmarshal created template ID")
    }

    d.SetId(t.TemplateID)

    items := d.Get("versions").([]interface{})

    for _, item := range items {
        i := item.(map[string]interface{})
        setVersions, err := setTemplateVersions(apiKey, d.Id(), i)
        if err != nil {
            return err
        }
    }

    d.SetPartial("versions")
    d.Partial(false)

    createStateConf := &resource.StateChangeConf{
        Pending:                   []string{statusWaiting},
        Target:                    []string{statusDone},
        Timeout:                   d.Timeout(schema.TimeoutCreate),
        Delay:                     defaultBackoff,
        MinTimeout:                defaultBackoff,
        ContinuousTargetOccurence: 3,
        Refresh: func() (interface{}, string, error) {
            template, err := getTemplate(apiKey, d.Id())
            if l, ok := err.(ratelimitError); ok {
                time.Sleep(l.timeout)
                return nil, statusWaiting, nil
            } else if err != nil {
                return nil, "", err
            } else if template == nil {
                return nil, statusWaiting, nil
            }

            return template, statusDone, nil
        },
    }

    _, err = createStateConf.WaitForState()
    if err != nil {
        return fmt.Errorf("error waiting for template (%s) to be created: %s", d.Id(), err)
    }

    return resourceTemplateRead(d, m)
}

func resourceTemplateRead(d *schema.ResourceData, m interface{}) error {
    apiKey := m.(*Config).APIKey
    template, err := getTemplate(apiKey, d.Id())
    if err != nil {
        return err
    } else if template == nil {
        d.SetId("")
        return nil
    }

    versionsItems := flattenVersions(template.Versions)
    if err := d.Set("versions", versionsItems); err != nil {
        return err
    }

    d.Set("name", template.Name)
    d.Set("versions", versionsItems)

    return nil
}

func resourceTemplateUpdate(d *schema.ResourceData, m interface{}) error {
    d.Partial(true)

    if d.HasChange("name") {
        if err := updateTemplateName(d, m); err != nil {
            return err
        }

        d.SetPartial("name")
    }

    if d.HasChange("versions") {
        apiKey := m.(*Config).APIKey
        items := d.Get("versions").([]interface{})
        for _, item := range items {
            i := item.(map[string]interface{})
            updateVersions, err := updateTemplateVersions(apiKey, d.Id(), i)
            if err != nil {
                return err
            }
        }

        d.SetPartial("versions")
    }

    d.Partial(false)

    return resourceTemplateRead(d, m)
}

func resourceTemplateDelete(d *schema.ResourceData, m interface {}) error {
    apiKey := m.(*Config).APIKey
    request := sendgrid.GetRequest(apiKey, "/v3/templates/"+d.Id(), sendgridAddress)
    request.Method = http.MethodDelete

    res, err := doRequest(request, withStatus(http.StatusNoContent), withRateLimit(deleteTemplateRate), withRetry(5))
    if err != nil || res.StatusCode != http.StatusOK {
        return nil
    }

    return errors.Wrap(err, "failed to delete template")
}

func getTemplate(apiKey, id string) (*template, error) {
    request := sendgrid.GetRequest(apiKey, "/v3/templates/"+id, sendgridAddress)
    request.Method = http.MethodGet

    res, err := doRequest(request, withStatus(http.StatusOK), withStatus(http.StatusNotFound))
    if err != nil {
        return nil, errors.Wrap(err, "failed to query API template")
    }

    if res.StatusCode == http.StatusNotFound {
        return nil, nil
    }

    var t template
    err = json.Unmarshal([]byte(res.Body), &t)
    if err != nil {
        return nil, errors.Wrap(err, "failed to unmarshal template query response")
    }

    return &t, nil
}

func parseTemplate(id string) (string, string, error) {
    parts := strings.SplitN(id, ":", 3)

    if len(parts) != 3 || parts[0] == "" || parts[1] == "" {
        return "", "", fmt.Errorf("unexpected format of ID (%s), expected id:name:versions", id)
    }

    realID := parts[0]
    name := parts[1]

    return realID, name, nil
}

func updateTemplateName(d *schema.ResourceData, m interface {}) error {
    name := d.Get("name").(string)

    payload := map[string]interface{}{
        "name": name,
    }

    data, err := json.Marshal(payload)
    if err != nil {
        return err
    }

    apiKey := m.(*Config).APIKey
    request := sendgrid.GetRequest(apiKey, "/v3/templates/"+d.Id(), sendgridAddress)
    request.Method = http.MethodPatch
    request.Body = data

    res, err := doRequest(request, withStatus(http.StatusCreated), withRateLimit(createTemplateRate))
    if err == nil || res.StatusCode != http.StatusOK {
        return nil
    }

    return nil
}

func setTemplateVersions(apiKey string, templateId string, v map[string]interface {}) (string, error) {
    uri := "/v3/templates/"+templateId+"/versions"

    payload := map[string]interface{}{
        "name": v["name"].(string),
        "subject": v["subject"].(string),
        "html_content": v["html_content"].(string),
        "plain_content": v["plain_content"].(string),
        "active": uint(v["active"].(int)),
        "template_id": templateId,
    }

    data, err := json.Marshal(payload)
    if err != nil {
        return "", errors.Wrap(err, "Invalid json set template versions")
    }

    request := sendgrid.GetRequest(apiKey, uri, sendgridAddress)
    request.Method = http.MethodPost
    request.Body = data

    res, err := doRequest(request, withStatus(http.StatusCreated), withRateLimit(deleteTemplateRate), withRetry(5))
    if err != nil {
        return "", errors.Wrap(err, "failed to create template versions")
    }

    var version versions
    err = json.Unmarshal([]byte(res.Body), &version)
    if err != nil {
        return "", errors.Wrap(err, "failed to unmarshal created template ID")
    }

    return version.Id, nil
}

func updateTemplateVersions(apiKey string, templateId string, v map[string]interface {}) (string, error) {
    uri := "/v3/templates/"+templateId+"/versions"

    payload := map[string]interface{}{
        "name": v["name"].(string),
        "subject": v["subject"].(string),
        "html_content": v["html_content"].(string),
        "plain_content": v["plain_content"].(string),
        "active": uint(v["active"].(int)),
        "template_id": templateId,
    }

    data, err := json.Marshal(payload)
    if err != nil {
        return "", errors.Wrap(err, "Invalid json update template versions")
    }

    request := sendgrid.GetRequest(apiKey, uri, sendgridAddress)
    request.Method = http.MethodPatch
    request.Body = data

    res, err := doRequest(request, withStatus(http.StatusCreated), withRateLimit(deleteTemplateRate), withRetry(5))
    if err != nil {
        return "", errors.Wrap(err, "failed to update template versions")
    }

    var version versions
    err = json.Unmarshal([]byte(res.Body), &version)
    if err != nil {
        return "", errors.Wrap(err, "failed to unmarshal created template ID")
    }
    
    return version.Id, nil
}

func flattenVersions(versionsItems []versions) []interface{} {
    if versionsItems != nil {
        vs := make([]interface{}, len(versionsItems), len(versionsItems))
        for i, versionsItem := range versionsItems {
            v := make(map[string]interface{})

            v["active"] = versionsItem.Active
            v["html_content"] = versionsItem.HtmlContent
            v["id"] = versionsItem.Id
            v["plain_content"] = versionsItem.PlainContent
            v["name"] = versionsItem.Name
            v["subject"] = versionsItem.Subject

            vs[i] = v
        }

        return vs
    }

    return make([]interface{}, 0)
}
