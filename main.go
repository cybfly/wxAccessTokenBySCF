package main

/*
package main
用于给应用获取服务号access_token的中控服务
1）应用（client）根据密钥对请求签名，并对接口发起请求
2）请求先通过API GW的密钥鉴权，验证通过后调用后端SCF函数
3）SCF函数通过上下文获得传入的参数信息，同时在启动时候根据环境变量进行系统初始化
4）实际业务执行后先判断该应用是否与当前服务号关联，无关联则退出
5）有关联先查询redis，判断access_token是否存在，不存在则重新获取，存在则直接从redis中返回
6）access_token获得后会存储在Redis中，超时时间由环境变量决定
7）如果请求里说明直接要重新刷新，则直接刷新
8) 应用请求时候通过Header来进行参数传输
	Source表示来源的应用(此处是tencent_visit);X-Date当前时间;Refresh是否刷新(为f表示先查询redis)
	req.Header.Set("Source", "tencent_visit")
	req.Header.Set("X-Date", dateTime)
	req.Header.Set("Refresh", "f")
9) 返回值,err不为nil,且可以从body中解析正确的{"access_token”:"tokenstr","expires_in":TTL}
10) 通过配置SCF的Timer可以触发定时刷新

注意:
1）假设当前redis中未过期，但发起了refresh重新刷新token又写入redis失败，会出现redis中与返回的token不一致的情况
会导致下次的非重新刷新可能获得到旧的不可用的token
2）由于是access_token，假设多个服务都请求来刷新token，会有可能导致A应用获得的tokenA，B又获得了新的tokenB，二者不一致
以最终生效的为准
3）由于是SCF，并发是多实例，那么难以避免同一时刻的请求会先后触发刷新access_token并存入redis的情况
4）可以跟应用方约束好，access_token不主动刷新，即不设置Refresh为t，而是通过中控定时刷新，然后应用方只拉取redis中的token跟ttl
   当TTL到期或者使用时候发现token失效则重新获取。这样出现异常的情况会比较少

测试用例：
1) Source不在范围内
2）IP不在白名单
3）Appid错误
4）APPSecret错误
5）tokenURL错误
6）Redis地址错误
7）Redis密码错误
8）refresh为t，连续执行
9）Refresh为f，连续执行
10）Sign构造成不一致
*/
import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/go-redis/redis"
	"github.com/tencentyun/scf-go-lib/cloudfunction"
	"github.com/tencentyun/scf-go-lib/events"
)

type runtime struct {
	queryClient *HttpClient    // httpClient用来发起微信服务请求
	redisClient *redis.Client  // 用来存取access_token，因为SCF无状态
	tokenUrl    string         // 微信服务号的请求URL
	appNames    map[string]int // 绑定该服务号的应用名白名单
	appid       string         // 服务号的appid
	appSecret   string         // 服务号的appSecret
	redisKey    string         // 存储在redis里的access_token的key名字
	expiresIn   int            // 存储在redis里的token的失效时间
	redisAddr   string         // redis服务器的IP:PORT
	redisPWD    string         // redis密码
}

type queryTokenBody struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   int    `json:"expires_in"`
	ErrCode     int    `json:"errcode"`
	ErrMsg      string `json:"errmsg"`
}

var g_runtime runtime

const (
	DefaultRedisAddr     = "172.16.16.2:6379"
	DefaultRedisPWD      = "XXX"
	DefaultTokenURL      = "https://api.weixin.qq.com/cgi-bin/token"
	DefaultAppid         = "XXX"
	DefaultAppSecret     = "XXX"
	DefaultDialTimeoutMs = 5000
	DefaultExpiresIn     = 300
)

func main() {
	initRuntime()

	g_runtime.redisClient = redis.NewClient(&redis.Options{
		Addr:     g_runtime.redisAddr,
		Password: g_runtime.redisPWD,
		DB:       0, // use default DB
	})
	defer g_runtime.redisClient.Close()
	_, err := g_runtime.redisClient.Ping().Result()
	if err != nil {
		fmt.Println("[WARN]failed to connect redis: ", err.Error())
	}

	g_runtime.queryClient = NewHttpClient(DefaultDialTimeoutMs, 10)

	// Make the handler available for Remote Procedure Call by Cloud Function
	cloudfunction.Start(runWork)
}

func runWorkDemo(ctx context.Context, event events.APIGatewayRequest) (*queryTokenBody, error) {
	token := &queryTokenBody{AccessToken: "123", ExpiresIn: 456}
	return token, nil
}

func initRuntime() {
	g_runtime.appid = os.Getenv("WX_APPID")
	g_runtime.appSecret = os.Getenv("WX_APP_SECRET")
	g_runtime.tokenUrl = os.Getenv("TOKEN_URL")
	g_runtime.redisAddr = os.Getenv("REDIS_ADDR")
	g_runtime.redisKey = os.Getenv("REDIS_KEY")
	g_runtime.redisPWD = os.Getenv("REDIS_PWD")
	appNames := os.Getenv("APP_NAMES")
	expiresIn, err := strconv.Atoi(os.Getenv("REDIS_EXPIRESIN"))
	if err != nil {
		expiresIn = DefaultExpiresIn
	}
	g_runtime.expiresIn = expiresIn

	g_runtime.appNames = make(map[string]int)
	// app1 app2 app3
	words := strings.Fields(appNames)
	for _, item := range words {
		g_runtime.appNames[item] = 0
	}

	if g_runtime.appid == "" {
		g_runtime.appid = DefaultAppid
	}
	if g_runtime.appSecret == "" {
		g_runtime.appSecret = DefaultAppSecret
	}
	if g_runtime.tokenUrl == "" {
		g_runtime.tokenUrl = DefaultTokenURL
	}
	if g_runtime.redisAddr == "" {
		g_runtime.redisAddr = DefaultRedisAddr
	}
	if g_runtime.redisPWD == "" {
		g_runtime.redisPWD = DefaultRedisPWD
	}

	g_runtime.tokenUrl = g_runtime.tokenUrl + "?grant_type=client_credential&appid=" + g_runtime.appid + "&secret=" + g_runtime.appSecret
}

func runWork(ctx context.Context, event events.APIGatewayRequest) (*queryTokenBody, error) {
	fmt.Println("[INFO]events> ", event)
	fmt.Println("[INFO]g_runtime> ", g_runtime)

	// if source not exist then skip check source
	if _, ok := event.Headers["source"]; ok {
		if _, ok := g_runtime.appNames[event.Headers["source"]]; !ok {
			fmt.Printf("[ERROR]app is not allowed: %s\n", event.Headers["source"])
			return nil, errors.New(`{"errcode":10001,"errmsg":"invalid app"}`)
		}
	}

	var access_token *queryTokenBody
	// whether refresh the access_token
	if event.Headers["refresh"] == "f" {
		access_token = queryToken()
	}

	if access_token == nil {
		return refreshToken()
	}
	return access_token, nil
}

// queryToken check whether token already storeid in redis, if not return ""
func queryToken() *queryTokenBody {
	val, err := g_runtime.redisClient.Get(g_runtime.redisKey).Result()
	if err == redis.Nil {
		fmt.Println("[INFO]access_token not in redis")
		return nil
	} else if err != nil {
		fmt.Println("[WARN]", err)
		return nil
	}

	ttl, err := g_runtime.redisClient.TTL(g_runtime.redisKey).Result()
	if err != nil {
		fmt.Println("[WARN]", err)
		return nil
	}

	fmt.Println("[INFO]access_token in redis, key: ", val, " ttl: ", ttl)
	//tokenStr := fmt.Sprintf(`{"access_token":"%s","expires_in":%d}`, val, int64(ttl.Seconds()))
	return &queryTokenBody{AccessToken: val, ExpiresIn: int(ttl.Seconds())}
}

// refreshToken get the tokenurl to get new access token
func refreshToken() (*queryTokenBody, error) {
	fmt.Println("[INFO]refresh access_token")

	resp, err := g_runtime.queryClient.GetTimeout(g_runtime.tokenUrl, DefaultDialTimeoutMs)
	if err != nil {
		fmt.Printf("[ERROR]refresh access_token err: %s\n", err.Error())
		return nil, err
	}
	// close the resp when no error
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		fmt.Printf("[ERROR]refresh access_token StatusCode: %d\n", resp.StatusCode)
		return nil, errors.New(`{"errcode":10002,"errmsg":"invalid resp.StatusCode: "` + strconv.Itoa(resp.StatusCode) + `}`)
	}

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		fmt.Printf("[ERROR]refresh access_token ioutil.ReadAll err: %s\n", err.Error())
		return nil, err
	}

	// 如果请求tokenURL时候有参数错误，返回值也是字符串，并不会报错
	// 比如Appid、AppSecret错误,所以需要先解析出返回体，看是否有返回access_token
	query_body := string(body)
	fmt.Println("[INFO]query tokenURL body: ", query_body)
	var data queryTokenBody
	if err := json.Unmarshal(body, &data); err != nil {
		fmt.Printf("unmarshal body err: %s\n", err.Error())
		return nil, err
	}

	// 此时是否应该清理redis？那多线程情况下是否有冲突？
	if data.AccessToken == "" {
		return nil, errors.New(query_body)
	}

	err = g_runtime.redisClient.Set(g_runtime.redisKey, data.AccessToken, time.Duration(g_runtime.expiresIn)*time.Second).Err()
	if err != nil {
		fmt.Printf("[WARN]refresh access_token redisClient.Set err: %s\n", err.Error())
	}

	data.ExpiresIn = g_runtime.expiresIn
	//tokenStr := fmt.Sprintf("{\"access_token\":\"%s\",\"expires_in\":%d}", data.AccessToken, g_runtime.expiresIn)
	//tokenStr := fmt.Sprintf(`{"access_token":"%s","expires_in":%d}`, data.AccessToken, g_runtime.expiresIn)
	//tokenStr, _ := json.Marshal(data)
	return &data, nil
}

/*
HttpClient
*/
type HttpClient struct {
	*http.Client
}

func NewHttpClient(dialTimeoutMs int, idleConns int) *HttpClient {
	if dialTimeoutMs <= 0 {
		dialTimeoutMs = DefaultDialTimeoutMs
	}

	if idleConns <= 0 {
		idleConns = 10
	}

	return &HttpClient{
		Client: &http.Client{
			Transport: &http.Transport{
				Dial: func(network, addr string) (net.Conn, error) {
					c, err := net.DialTimeout(network, addr, time.Millisecond*time.Duration(dialTimeoutMs))
					if err != nil {
						return nil, err
					}
					return c, nil
				},
				MaxIdleConnsPerHost: idleConns,

				TLSClientConfig: &tls.Config{
					InsecureSkipVerify: true,
				},
			},
		},
	}
}

func (cli *HttpClient) DoTimeout(req *http.Request, timeoutMs int) (rsp *http.Response, err error) {
	if timeoutMs <= 0 {
		return cli.Do(req)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeoutMs)*time.Millisecond)

	defer func() {
		cancel()
	}()

	return cli.Do(req.WithContext(ctx))
}

func (cli *HttpClient) GetTimeout(url string, timeoutMs int) (*http.Response, error) {
	if url == "" {
		return nil, errors.New("empty url")
	}

	if timeoutMs <= 0 {
		return cli.Get(url)
	}

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}

	return cli.DoTimeout(req, timeoutMs)
}
